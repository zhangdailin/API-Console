package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// mergeModelCooldowns combines two per-model cooldown maps, keeping the later
// deadline for each model and discarding entries that have already expired.
func mergeModelCooldowns(existing, incoming map[string]time.Time) map[string]time.Time {
	if len(existing) == 0 && len(incoming) == 0 {
		return nil
	}
	now := time.Now()
	merged := make(map[string]time.Time, len(existing)+len(incoming))
	for _, source := range []map[string]time.Time{existing, incoming} {
		for model, until := range source {
			name := strings.TrimSpace(model)
			if name == "" || until.IsZero() || !until.After(now) {
				continue
			}
			if current, ok := merged[name]; !ok || until.After(current) {
				merged[name] = until
			}
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// mergeModelCooldownReasons keeps a reason only while the deadline it describes
// is alive, so a label cannot outlive its cooldown. A writer records the deadline
// and its reason together, which makes the incoming label the right one exactly
// when the incoming deadline survived the merge; when the stored deadline
// outlasts it, the stored label still describes what is on disk.
func mergeModelCooldownReasons(existing, incoming map[string]ModelCooldownReason, existingUntil, incomingUntil map[string]time.Time) map[string]ModelCooldownReason {
	if len(existing) == 0 && len(incoming) == 0 {
		return nil
	}
	now := time.Now()
	merged := make(map[string]ModelCooldownReason, len(existing)+len(incoming))
	for model, reason := range existing {
		name := strings.TrimSpace(model)
		until := existingUntil[name]
		if name == "" || until.IsZero() || !until.After(now) {
			continue
		}
		if known := knownModelCooldownReason(reason); known != "" {
			merged[name] = known
		}
	}
	for model, reason := range incoming {
		name := strings.TrimSpace(model)
		until, ok := incomingUntil[name]
		if name == "" || !ok || until.IsZero() || !until.After(now) {
			continue
		}
		if stored, had := existingUntil[name]; had && stored.After(until) {
			continue
		}
		if known := knownModelCooldownReason(reason); known != "" {
			merged[name] = known
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// knownModelCooldownReason returns the reason when it is one this build
// understands, and "" otherwise so the caller falls back to the deadline.
func knownModelCooldownReason(reason ModelCooldownReason) ModelCooldownReason {
	switch reason {
	case ModelCooldownThrottled, ModelCooldownUnavailable:
		return reason
	default:
		return ""
	}
}

// SetModelCooldown changes only the specified model on the credential generation
// that saw the refusal. An in-flight request cannot restore operator settings.
func (s *Store) SetModelCooldown(ctx context.Context, observed *Account, model string, until time.Time, reason ModelCooldownReason) error {
	if s == nil || s.client == nil || observed == nil {
		return fmt.Errorf("model cooldown store is unavailable")
	}
	return s.updateAccountAtomic(ctx, observed.ID, func(current *Account) error {
		if !current.Enabled || !sameCooldownCredential(current, observed) {
			return errAccountUnchanged
		}
		current.ModelCooldowns = mergeModelCooldowns(nil, current.ModelCooldowns)
		current.ModelCooldownReasons = mergeModelCooldownReasons(nil, current.ModelCooldownReasons, nil, current.ModelCooldowns)
		RecordModelCooldownWithReason(current, model, until, reason)
		return nil
	}, true)
}

func sameCooldownCredential(current, observed *Account) bool {
	if current.AccountType != observed.AccountType || current.UserID != observed.UserID {
		return false
	}
	before := accountCredentialFields(observed)
	for i, field := range accountCredentialFields(current) {
		if *field != *before[i] {
			return false
		}
	}
	return true
}

// RecordModelCooldown marks one model of an account as throttled until the given
// deadline. Only the named model is affected: the account stays in the pool for
// its other models, which is the difference between "this model is hot" and
// "this account is dead".
func RecordModelCooldown(acc *Account, model string, until time.Time) {
	RecordModelCooldownWithReason(acc, model, until, ModelCooldownThrottled)
}

// RecordModelCooldownWithReason is RecordModelCooldown plus the verdict the
// deadline came from. The reason is what lets the selection layer tell a throttle
// from a model this account's plan does not cover: one is worth retrying, the
// other never becomes true by waiting.
func RecordModelCooldownWithReason(acc *Account, model string, until time.Time, reason ModelCooldownReason) {
	if acc == nil || until.IsZero() || !until.After(time.Now()) {
		return
	}
	name := strings.TrimSpace(model)
	if name == "" {
		return
	}
	if acc.ModelCooldowns == nil {
		acc.ModelCooldowns = map[string]time.Time{}
	}
	if current, ok := acc.ModelCooldowns[name]; ok && !until.After(current) {
		// The stored deadline outlasts this one, so its reason still describes
		// what is on disk: a shorter throttle must not relabel a day-long plan
		// verdict.
		return
	}
	acc.ModelCooldowns[name] = until
	if acc.ModelCooldownReasons == nil {
		acc.ModelCooldownReasons = map[string]ModelCooldownReason{}
	}
	if known := knownModelCooldownReason(reason); known != "" {
		acc.ModelCooldownReasons[name] = known
	} else {
		acc.ModelCooldownReasons[name] = ModelCooldownThrottled
	}
}

// ModelCooldownRemaining reports how long the account is throttled for one model,
// or zero when it may be used. It is the single reader of ModelCooldowns, so the
// pool and the request path agree on what "cooling down" means.
func ModelCooldownRemaining(acc *Account, model string, now time.Time) time.Duration {
	if acc == nil || len(acc.ModelCooldowns) == 0 {
		return 0
	}
	until, ok := acc.ModelCooldowns[strings.TrimSpace(model)]
	if !ok || until.IsZero() || !until.After(now) {
		return 0
	}
	return until.Sub(now)
}

// ModelCooldownKind reports what one model's cooldown on this account means, or
// "" when the model is not cooling down at all.
//
// A deadline recorded before reasons were stored carries no label and the
// deadline itself is then the only signal left. That fallback reads a long hold
// as a plan verdict because the two verdicts that create a handler-path model
// cooldown are a 30s throttle and a day-long plan refusal; anything still running
// after an hour cannot be the throttle. Channels that hold a model for their own
// windows (a free-usage window, an inference cap) are unaffected: they do not
// reach this reader, their filters answer with their own reasons.
func ModelCooldownKind(acc *Account, model string, now time.Time) ModelCooldownReason {
	if acc == nil {
		return ""
	}
	name := strings.TrimSpace(model)
	remaining := ModelCooldownRemaining(acc, name, now)
	if remaining <= 0 {
		return ""
	}
	if known := knownModelCooldownReason(acc.ModelCooldownReasons[name]); known != "" {
		return known
	}
	if remaining > ModelCooldownEntitlementFloor {
		return ModelCooldownUnavailable
	}
	return ModelCooldownThrottled
}
