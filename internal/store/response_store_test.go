package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/testutil"
)

func TestStoredResponseOwnershipLifecycleAndIsolation(t *testing.T) {
	s, _ := newTestRedisStore(t, "responses-test:")

	ctx := context.Background()
	record := &StoredResponse{
		ResponseID: "resp_123", OwnerHash: "owner-a", AccountID: 42,
		Model: "grok-4.6", Provider: "build",
	}
	testutil.NoError(t, s.SaveStoredResponse(ctx, record, time.Hour), "SaveStoredResponse() error = %v")
	got, err := s.GetStoredResponse(ctx, "resp_123", "owner-a")
	testutil.Falsef(t, err != nil || got.AccountID != 42 || got.Model != "grok-4.6" || got.ExpiresAt.IsZero(), "GetStoredResponse() = %#v, %v", got, err)
	_, err = s.GetStoredResponse(ctx, "resp_123", "owner-b")
	testutil.Falsef(t, !errors.Is(err, ErrNoRows), "cross-owner lookup error = %v, want ErrNoRows", err)
	testutil.NoError(t, s.DeleteStoredResponse(ctx, "resp_123", "owner-a"), "DeleteStoredResponse() error = %v")
	_, err = s.GetStoredResponse(ctx, "resp_123", "owner-a")
	testutil.Falsef(t, !errors.Is(err, ErrNoRows), "lookup after delete error = %v, want ErrNoRows", err)
}

func TestStoredResponseDefaultTTLandIdentityAcrossBackends(t *testing.T) {
	redis, _ := newTestRedisStore(t, "responses-default:")
	type responseStore interface {
		SaveStoredResponse(context.Context, *StoredResponse, time.Duration) error
		GetStoredResponseMetadata(context.Context, string, string) (*StoredResponse, error)
	}
	for name, backend := range map[string]responseStore{
		"redis": redis, "memory": NewMemoryResponseStore(0),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			for _, invalid := range []*StoredResponse{nil, {ResponseID: "resp", OwnerHash: " "}, {ResponseID: " ", OwnerHash: "owner"}} {
				if err := backend.SaveStoredResponse(ctx, invalid, 0); !errors.Is(err, ErrStoredResponseIdentityRequired) {
					t.Fatalf("accepted invalid response identity: %v", err)
				}
			}

			original := &StoredResponse{ResponseID: " resp ", OwnerHash: " owner ", CreatedAt: time.Unix(100, 0).UTC()}
			if err := backend.SaveStoredResponse(ctx, original, 0); err != nil {
				t.Fatal(err)
			}
			got, err := backend.GetStoredResponseMetadata(ctx, "resp", "owner")
			if err != nil {
				t.Fatal(err)
			}
			if got.ResponseID != "resp" || got.OwnerHash != "owner" || !got.CreatedAt.Equal(original.CreatedAt) ||
				got.UpdatedAt.IsZero() || got.ExpiresAt.Sub(got.UpdatedAt) != 30*24*time.Hour {
				t.Fatalf("unexpected stored record: %#v", got)
			}
			if original.ResponseID != " resp " || original.OwnerHash != " owner " || !original.UpdatedAt.IsZero() {
				t.Fatalf("saving mutated the caller's record: %#v", original)
			}
		})
	}
}

func TestStoredResponseExpires(t *testing.T) {
	s, mini := newTestRedisStore(t, "responses-expiry:")
	if err := s.SaveStoredResponse(context.Background(), &StoredResponse{
		ResponseID: "resp_expiring", OwnerHash: "owner", AccountID: 1,
	}, time.Second); err != nil {
		t.Fatal(err)
	}
	mini.FastForward(2 * time.Second)
	_, err := s.GetStoredResponse(context.Background(), "resp_expiring", "owner")
	testutil.Falsef(t, !errors.Is(err, ErrNoRows), "expired lookup error = %v, want ErrNoRows", err)
}

func TestStoredResponseFilesAreSharedOnlyWithinOneOwner(t *testing.T) {
	s, mini := newTestRedisStore(t, "responses-files:")
	ctx := t.Context()
	source := "DATA:application/pdf;BASE64," + strings.Repeat("A", 1<<15)
	raw := json.RawMessage(`[{"role":"user","content":[{"type":"INPUT_FILE","file":{"filename":"report.pdf","data":"` + source + `"}}]}]`)
	save := func(id, owner string, ttl time.Duration, externalize bool) {
		t.Helper()
		err := s.SaveStoredResponse(ctx, &StoredResponse{ResponseID: id, OwnerHash: owner, InputItems: raw, ExternalizeInputFiles: externalize}, ttl)
		if err != nil {
			t.Fatal(err)
		}
	}
	save("legacy", "a", time.Minute, false)
	save("first", "a", time.Second, true)
	save("second", "a", 2*time.Minute, true)
	save("separate", "b", time.Minute, true)
	var files, compactRows int
	for _, key := range mini.Keys() {
		switch {
		case strings.Contains(key, ":input-file:"):
			files++
		case strings.Contains(key, ":ownership:"):
			value, err := mini.Get(key)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(value, source) {
				compactRows++
			}
		}
	}
	if files != 2 || compactRows != 3 {
		t.Fatalf("stored %d file blobs and %d compact rows, want 2 and 3", files, compactRows)
	}
	if err := s.DeleteStoredResponse(ctx, "first", "a"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"legacy", "second"} {
		got, err := s.GetStoredResponse(ctx, id, "a")
		if err != nil || !bytes.Contains(got.InputItems, []byte(source)) || len(got.InputFileRefs) != 0 {
			t.Fatalf("%s could not read its file: %v", id, err)
		}
	}
	if _, err := s.GetStoredResponse(ctx, "second", "b"); !errors.Is(err, ErrNoRows) {
		t.Fatalf("another owner read the response: %v", err)
	}
	mini.FastForward(70 * time.Second)
	if got, err := s.GetStoredResponse(ctx, "second", "a"); err != nil || !bytes.Contains(got.InputItems, []byte(source)) {
		t.Fatalf("a longer living response lost its file: %v", err)
	}
}

func TestMetadataAndBodyUpdateDoNotDependOnAttachment(t *testing.T) {
	s, mini := newTestRedisStore(t, "response-meta:")
	ctx := t.Context()
	row := &StoredResponse{ResponseID: "fixture", OwnerHash: "owner",
		Body: []byte(`{"status":"completed"}`), ExternalizeInputFiles: true,
		InputItems: json.RawMessage(`[{"content":[{"type":"input_file","file_data":"data:application/pdf;base64,JVBERg=="}]}]`),
	}
	if err := s.SaveStoredResponse(ctx, row, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, key := range mini.Keys() {
		if strings.Contains(key, ":input-file:") {
			mini.Del(key)
		}
	}
	if _, err := s.GetStoredResponse(ctx, row.ResponseID, row.OwnerHash); !errors.Is(err, errInvalidResponseInputFile) {
		t.Fatalf("history read ignored missing attachment: %v", err)
	}
	prior, err := s.GetStoredResponseMetadata(ctx, row.ResponseID, row.OwnerHash)
	if err != nil || len(prior.InputFileRefs) != 1 {
		t.Fatalf("metadata read: %v", err)
	}
	if err := s.UpdateStoredResponseBody(ctx, prior, []byte(`{"status":"cancelled"}`)); err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.GetStoredResponseMetadata(ctx, row.ResponseID, row.OwnerHash)
	if err != nil || !bytes.Contains(cancelled.Body, []byte("cancelled")) || !cancelled.ExpiresAt.Equal(prior.ExpiresAt) || len(cancelled.InputFileRefs) != 1 {
		t.Fatalf("body update rewrote ownership or expiry: %v", err)
	}
	if err := s.UpdateStoredResponseBody(ctx, prior, nil); !errors.Is(err, ErrStoredResponseChanged) {
		t.Fatalf("stale update not rejected: %v", err)
	}
	if err := s.DeleteStoredResponse(ctx, row.ResponseID, row.OwnerHash); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStoredResponseBody(ctx, cancelled, []byte(`{"status":"cancelled"}`)); !errors.Is(err, ErrNoRows) {
		t.Fatalf("updated a deleted row: %v", err)
	}
}
