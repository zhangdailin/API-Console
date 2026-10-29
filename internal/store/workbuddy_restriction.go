package store

import (
	"strings"
	"time"

	apperrors "orchids-api/internal/errors"
)

// Preserve the existing WorkBuddy 403 cooldown. This is a local retry window,
// not a promise that the upstream will lift the restriction after this time.
const WorkBuddyRestrictionCooldown = 24 * time.Hour

const WorkBuddyRestrictionMessage = "workbuddy API error: status=403, code=11140, message=account restricted due to unusual activity"

// WorkBuddyRestrictionUntil also reads pre-upgrade 403/11140 records without
// resetting their clock or requiring a Redis migration.
func WorkBuddyRestrictionUntil(acc *Account) time.Time {
	if acc == nil || !strings.EqualFold(acc.AccountType, "workbuddy") {
		return time.Time{}
	}
	if !acc.WorkBuddyRestrictedUntil.IsZero() {
		return acc.WorkBuddyRestrictedUntil
	}
	if acc.StatusCode == "403" && apperrors.IsWorkBuddyRestricted(acc.StatusMessage) && !acc.LastAttempt.IsZero() {
		return acc.LastAttempt.Add(WorkBuddyRestrictionCooldown)
	}
	return time.Time{}
}

func (p accountPatch) mergeWorkBuddyRestriction() {
	u, acc, existing := p.updated, p.acc, p.existing
	if !strings.EqualFold(u.AccountType, "workbuddy") {
		return
	}
	until := WorkBuddyRestrictionUntil(existing)
	if !until.After(time.Now()) {
		if next := WorkBuddyRestrictionUntil(acc); next.After(until) {
			until = next
		}
	}
	u.WorkBuddyRestrictedUntil = until
	if until.After(time.Now()) {
		// A catalog/quota success, credential edit or stale concurrent writer
		// cannot prove that the chat restriction was lifted.
		u.StatusCode = "403"
		u.StatusMessage = WorkBuddyRestrictionMessage
		u.LastAttempt = until.Add(-WorkBuddyRestrictionCooldown)
	}
}
