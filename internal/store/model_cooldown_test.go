package store

import (
	"encoding/json"
	"orchids-api/internal/testutil"
	"testing"
	"time"
)

// TestModelCooldown_ScopedToModelNotAccount is the rule the pool depends on: a
// throttled model must be skipped for that model while the account stays usable
// for every other model.
func TestModelCooldown_ScopedToModelNotAccount(t *testing.T) {
	now := time.Now()
	acc := &Account{
		AccountType: "grok",
		ModelCooldowns: map[string]time.Time{
			"grok-4.6":     now.Add(90 * time.Second),
			"grok-heavy":   now.Add(time.Hour),
			"grok-expired": now.Add(-time.Minute),
		},
	}

	got := ModelCooldownRemaining(acc, "grok-4.6", now)
	testutil.Falsef(t, got <= 0, "throttled model remaining = %v, want > 0", got)
	testutil.Equal(t, ModelCooldownRemaining(acc, "grok-4.5", now), 0)
	testutil.Equal(t, ModelCooldownRemaining(acc, "grok-expired", now), 0)
	testutil.Equal(t, ModelCooldownRemaining(nil, "grok-4.6", now), 0)
}

// TestRecordModelCooldown_SetsOnlyThatModel keeps the write path honest: a
// throttle for one model must never mark the account or another model.
func TestRecordModelCooldown_SetsOnlyThatModel(t *testing.T) {
	now := time.Now()
	acc := &Account{AccountType: "grok"}

	RecordModelCooldown(acc, "grok-4.6", now.Add(time.Minute))
	remaining := ModelCooldownRemaining(acc, "grok-4.6", now)
	testutil.False(t, remaining <= 0, "the throttled model must be marked")
	testutil.Equal(t, ModelCooldownRemaining(acc, "grok-4.5", now), 0)
	testutil.Equal(t, acc.StatusCode, "")

	// A later deadline wins; an earlier one does not shorten it.
	RecordModelCooldown(acc, "grok-4.6", now.Add(5*time.Minute))
	got := ModelCooldownRemaining(acc, "grok-4.6", now)
	testutil.Falsef(t, got < 4*time.Minute, "remaining = %v, want the later deadline", got)
	RecordModelCooldown(acc, "grok-4.6", now.Add(10*time.Second))
	got = ModelCooldownRemaining(acc, "grok-4.6", now)
	testutil.Falsef(t, got < 4*time.Minute, "an earlier deadline shortened the cooldown to %v", got)

	// A past deadline is not recorded at all.
	before := len(acc.ModelCooldowns)
	RecordModelCooldown(acc, "grok-4.7", now.Add(-time.Minute))
	testutil.Equal(t, len(acc.ModelCooldowns), before)
	RecordModelCooldown(acc, "  ", now.Add(time.Minute))
	testutil.Equal(t, len(acc.ModelCooldowns), before)
}

// TestMergeModelCooldowns_KeepsLatestAndDropsExpired covers the write path: a
// partial update must not erase a cooldown another path just recorded, and dead
// entries must not accumulate.
func TestMergeModelCooldowns_KeepsLatestAndDropsExpired(t *testing.T) {
	now := time.Now()
	merged := mergeModelCooldowns(
		map[string]time.Time{
			"grok-4.6":   now.Add(30 * time.Second),
			"grok-stale": now.Add(-time.Hour),
		},
		map[string]time.Time{
			"grok-4.6": now.Add(5 * time.Minute),
			"grok-4.5": now.Add(time.Minute),
			"":         now.Add(time.Hour),
		},
	)
	testutil.Equal(t, len(merged), 2)
	if merged["grok-4.6"].Sub(now) < 4*time.Minute {
		t.Fatalf("grok-4.6 = %v, want the later deadline", merged["grok-4.6"].Sub(now))
	}
	_, dead := merged["grok-stale"]
	testutil.False(t, dead, "an expired cooldown must not be carried forward")
	testutil.False(t, mergeModelCooldowns(nil, nil) != nil, "empty input must not create an empty map")
}

// TestUpdateAccount_PreservesModelCooldownsAcrossPartialWrites pins the guard:
// saving an unrelated field (enabled, weight) keeps cooldowns in Redis.
func TestUpdateAccount_PreservesModelCooldownsAcrossPartialWrites(t *testing.T) {
	s, _ := newTestRedisStore(t, "modelcool:")

	ctx := t.Context()
	acc := &Account{
		AccountType:    "grok",
		Enabled:        true,
		ModelCooldowns: map[string]time.Time{"grok-4.6": time.Now().Add(2 * time.Minute)},
	}
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")

	// A request path that knows nothing about model cooldowns saves the account.
	stale, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	stale.Weight = 3
	stale.ModelCooldowns = nil
	testutil.NoError(t, s.UpdateAccount(ctx, stale), "UpdateAccount() error = %v")

	after, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Equal(t, after.Weight, 3)
	remaining := ModelCooldownRemaining(after, "grok-4.6", time.Now())
	testutil.False(t, remaining <= 0, "a partial write erased the model cooldown")
}

// TestModelCooldownKind_SeparatesPlanFromThrottle pins what the selection layer
// reads. The pool can see a cooled model but not the verdict behind it, so the
// label decides whether an emptied pool invites a retry or reports a model no
// matching account can serve.
func TestModelCooldownKind_SeparatesPlanFromThrottle(t *testing.T) {
	now := time.Now()
	acc := &Account{AccountType: "cline"}
	RecordModelCooldownWithReason(acc, "glm-5.3", now.Add(24*time.Hour), ModelCooldownUnavailable)
	RecordModelCooldownWithReason(acc, "efficient", now.Add(30*time.Second), ModelCooldownThrottled)

	testutil.Equal(t, ModelCooldownKind(acc, "glm-5.3", now), ModelCooldownUnavailable)
	testutil.Equal(t, ModelCooldownKind(acc, "efficient", now), ModelCooldownThrottled)
	testutil.Equal(t, ModelCooldownKind(acc, "untouched", now), "")
	testutil.Equal(t, ModelCooldownKind(nil, "glm-5.3", now), "")

	// A deadline recorded before reasons were stored is read from the deadline:
	// a day-long hold cannot be the 30s throttle.
	legacy := &Account{ModelCooldowns: map[string]time.Time{
		"day-long": now.Add(27 * time.Hour),
		"short":    now.Add(20 * time.Second),
	}}
	testutil.Equal(t, ModelCooldownKind(legacy, "day-long", now), ModelCooldownUnavailable)
	testutil.Equal(t, ModelCooldownKind(legacy, "short", now), ModelCooldownThrottled)

	// An expired cooldown is not cooling down at all, label or not.
	expired := &Account{
		ModelCooldowns:       map[string]time.Time{"gone": now.Add(-time.Minute)},
		ModelCooldownReasons: map[string]ModelCooldownReason{"gone": ModelCooldownUnavailable},
	}
	testutil.Equal(t, ModelCooldownKind(expired, "gone", now), "")
}

// TestRecordModelCooldownWithReason_KeepsTheStoredVerdict pins that a shorter
// hold cannot relabel a longer one: a 30s throttle arriving after a day-long plan
// verdict must not turn it back into "retry later".
func TestRecordModelCooldownWithReason_KeepsTheStoredVerdict(t *testing.T) {
	now := time.Now()
	acc := &Account{AccountType: "cline"}

	RecordModelCooldownWithReason(acc, "glm-5.3", now.Add(24*time.Hour), ModelCooldownUnavailable)
	RecordModelCooldown(acc, "glm-5.3", now.Add(30*time.Second))
	testutil.Equal(t, ModelCooldownKind(acc, "glm-5.3", now), ModelCooldownUnavailable)

	RecordModelCooldown(acc, "efficient", now.Add(30*time.Second))
	RecordModelCooldownWithReason(acc, "efficient", now.Add(24*time.Hour), ModelCooldownUnavailable)
	testutil.Equal(t, ModelCooldownKind(acc, "efficient", now), ModelCooldownUnavailable)

	// An unrecognised label falls back to the deadline rather than being trusted.
	odd := &Account{
		ModelCooldowns:       map[string]time.Time{"m": now.Add(24 * time.Hour)},
		ModelCooldownReasons: map[string]ModelCooldownReason{"m": "something-else"},
	}
	testutil.Equal(t, ModelCooldownKind(odd, "m", now), ModelCooldownUnavailable)
}

// TestMergeModelCooldownReasons_FollowTheirDeadline keeps labels attached to the
// deadline they describe, and drops the ones whose deadline is gone.
func TestMergeModelCooldownReasons_FollowTheirDeadline(t *testing.T) {
	now := time.Now()

	// The stored deadline outlasts the incoming one, so the stored label stands.
	merged := mergeModelCooldownReasons(
		map[string]ModelCooldownReason{"m": ModelCooldownUnavailable},
		map[string]ModelCooldownReason{"m": ModelCooldownThrottled},
		map[string]time.Time{"m": now.Add(24 * time.Hour)},
		map[string]time.Time{"m": now.Add(30 * time.Second)},
	)
	testutil.Equal(t, merged["m"], ModelCooldownUnavailable)

	// The incoming deadline wins, so its label does too.
	merged = mergeModelCooldownReasons(
		map[string]ModelCooldownReason{"m": ModelCooldownThrottled},
		map[string]ModelCooldownReason{"m": ModelCooldownUnavailable},
		map[string]time.Time{"m": now.Add(30 * time.Second)},
		map[string]time.Time{"m": now.Add(24 * time.Hour)},
	)
	testutil.Equal(t, merged["m"], ModelCooldownUnavailable)

	if got := mergeModelCooldownReasons(
		map[string]ModelCooldownReason{"dead": ModelCooldownUnavailable},
		nil,
		map[string]time.Time{"dead": now.Add(-time.Minute)},
		nil,
	); got != nil {
		t.Fatalf("merged = %v, want nil: a label must not outlive its deadline", got)
	}
	got := mergeModelCooldownReasons(nil, nil, nil, nil)
	testutil.Falsef(t, got != nil, "merged = %v, want nil for empty input", got)
}

// TestModelCooldownReasonsSurviveRedis covers the persistence contract: the label
// rides along with the deadline through partial writes, and the deadline map keeps
// the shape earlier binaries wrote and read, so a rollback cannot turn a cooled
// account into an undecodable document.
func TestModelCooldownReasonsSurviveRedis(t *testing.T) {
	s, _ := newTestRedisStore(t, "modelcoolreason:")
	ctx := t.Context()

	acc := &Account{AccountType: "cline", Enabled: true}
	RecordModelCooldownWithReason(acc, "glm-5.3", time.Now().Add(24*time.Hour), ModelCooldownUnavailable)
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")

	// A path that knows nothing about cooldowns saves the account.
	stale, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	stale.Weight = 2
	stale.ModelCooldowns = nil
	stale.ModelCooldownReasons = nil
	testutil.NoError(t, s.UpdateAccount(ctx, stale), "UpdateAccount() error = %v")

	after, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Equal(t, ModelCooldownKind(after, "glm-5.3", time.Now()), ModelCooldownUnavailable)

	// The deadline map still decodes exactly as an earlier binary wrote it.
	var legacy Account
	testutil.NoError(t, json.Unmarshal([]byte(`{"id":9,"account_type":"cline","model_cooldowns":{"m":"2030-01-01T00:00:00Z"}}`), &legacy), "legacy account document no longer decodes: %v")
	testutil.Equal(t, len(legacy.ModelCooldowns), 1)

	// And the other direction: an earlier binary reading a document this build
	// wrote. It knows only the deadline map, and encoding/json skips the sibling
	// field it does not define, which is what makes a rollback safe.
	raw, err := json.Marshal(after)
	testutil.NoError(t, err, "Marshal() error = %v")
	var previousBuild struct {
		ModelCooldowns map[string]time.Time `json:"model_cooldowns,omitempty"`
	}
	testutil.NoError(t, json.Unmarshal(raw, &previousBuild), "a build that only knows model_cooldowns cannot read this document: %v")
	testutil.Equal(t, len(previousBuild.ModelCooldowns), 1)
}
