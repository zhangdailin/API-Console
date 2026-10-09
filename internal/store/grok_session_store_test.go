package store

import (
	"context"
	"encoding/json"
	"errors"
	"orchids-api/internal/testutil"
	"reflect"
	"testing"
	"time"
)

func TestReasoningReplayItemsPersistenceAndExpiry(t *testing.T) {
	s, mini := newTestRedisStore(t, "grok-replay-items-test:")
	ctx := context.Background()
	items := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","summary":[],"encrypted_content":"cipher-a"}`),
		json.RawMessage(`{"type":"reasoning","summary":[{"type":"summary_text","text":"step"}],"encrypted_content":"cipher-b"}`),
	}
	original := &StoredReasoningReplay{Model: "grok-4.6", SessionKey: "session-items", Items: items}
	testutil.NoError(t, s.SaveReasoningReplay(ctx, original, 2*time.Second), "SaveReasoningReplay(items) error = %v")
	testutil.False(t, !original.ExpiresAt.IsZero(), "SaveReasoningReplay mutated caller expiry")
	replay, err := s.GetReasoningReplay(ctx, original.Model, original.SessionKey)
	testutil.Falsef(t, err != nil || replay == nil || !reflect.DeepEqual(replay.Items, items), "GetReasoningReplay(items) = %#v, %v", replay, err)
	testutil.Falsef(t, replay.ExpiresAt.IsZero() || time.Until(replay.ExpiresAt) <= 0, "missing future replay expiry: %v", replay.ExpiresAt)
	mini.FastForward(3 * time.Second)
	replay, err = s.GetReasoningReplay(ctx, original.Model, original.SessionKey)
	testutil.Falsef(t, !errors.Is(err, ErrNoRows) || replay != nil, "GetReasoningReplay(expired items) = %#v, %v; want ErrNoRows", replay, err)
}

func TestReasoningReplaySaveValidation(t *testing.T) {
	s, _ := newTestRedisStore(t, "grok-replay-validation-test:")
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		items []json.RawMessage
	}{
		{name: "missing items"},
		{name: "empty item", items: []json.RawMessage{nil}},
		{name: "whitespace", items: []json.RawMessage{json.RawMessage("  \n  ")}},
		{name: "malformed", items: []json.RawMessage{json.RawMessage(`{"type":`)}},
		{name: "null", items: []json.RawMessage{json.RawMessage(`null`)}},
		{name: "scalar", items: []json.RawMessage{json.RawMessage(`"cipher"`)}},
		{name: "number", items: []json.RawMessage{json.RawMessage(`42`)}},
		{name: "array", items: []json.RawMessage{json.RawMessage(`[{"type":"reasoning"}]`)}},
		{name: "mixed valid and invalid", items: []json.RawMessage{json.RawMessage(`{"type":"reasoning"}`), json.RawMessage(`false`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := "invalid-" + tc.name
			if err := s.SaveReasoningReplay(ctx, &StoredReasoningReplay{
				Model: "grok-4.6", SessionKey: key, Items: tc.items,
			}, time.Hour); err == nil {
				t.Fatal("SaveReasoningReplay() accepted invalid items")
			}
			replay, err := s.GetReasoningReplay(ctx, "grok-4.6", key)
			testutil.Falsef(t, !errors.Is(err, ErrNoRows) || replay != nil, "GetReasoningReplay(invalid) = %#v, %v; want ErrNoRows", replay, err)
		})
	}

}

func TestReasoningReplayAndSessionAffinityLifecycle(t *testing.T) {
	s, _ := newTestRedisStore(t, "grok-session-test:")
	ctx := context.Background()
	if err := s.SaveReasoningReplay(ctx, &StoredReasoningReplay{
		Model: "grok-4.6", SessionKey: "session-a", Items: []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque"}`)},
	}, time.Hour); err != nil {
		t.Fatalf("SaveReasoningReplay() error = %v", err)
	}
	replay, err := s.GetReasoningReplay(ctx, "grok-4.6", "session-a")
	testutil.Equal(t, err, nil)
	testutil.Equal(t, len(replay.Items), 1)
	_, err = s.GetReasoningReplay(ctx, "grok-4.5", "session-a")
	testutil.Falsef(t, !errors.Is(err, ErrNoRows), "cross-model replay err=%v", err)

	if err := s.SaveSessionAffinity(ctx, &StoredSessionAffinity{
		Provider: "build", Model: "grok-4.6", SessionKey: "session-a", AccountID: 42,
	}, time.Hour); err != nil {
		t.Fatalf("SaveSessionAffinity() error = %v", err)
	}
	affinity, err := s.GetSessionAffinity(ctx, "build", "grok-4.6", "session-a")
	testutil.Equal(t, err, nil)
	testutil.Equal(t, affinity.AccountID, 42)
	_, err = s.GetSessionAffinity(ctx, "console", "grok-4.6", "session-a")
	testutil.Falsef(t, !errors.Is(err, ErrNoRows), "cross-provider affinity err=%v", err)
}
