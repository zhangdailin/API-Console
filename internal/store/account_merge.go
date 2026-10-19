package store

import (
	"strings"
	"time"

	"orchids-api/internal/modelcatalog"
)

// UpdateAccount merges a partial Account into the stored one. Callers hand it a
// snapshot that may only carry the fields they mean to write — a request
// counter, a quota refresh, a rotated credential — so almost every field is
// guarded rather than copied, and the guard is what keeps concurrent writers
// from erasing each other's work.
//
// The guards are grouped below into one step per subject, each a method on
// accountPatch, so the merge reads as a list of rules:
//
//	mergeIdentity               name, type, weight, concurrency, enabled
//	mergeUsageMeter             generic usage slots and the WorkBuddy meter
//	mergeStatusAndVerdict       status, verdict timestamps, quota reset
//	mergeGrokCredentials        Grok OAuth identity and provider snapshots
//	mergeModelCooldownState     per-model cooldowns and their labels
//	mergeWorkBuddyCredentials   WorkBuddy credentials and catalog
//	mergeQoderCredentials       Qoder credentials, organization and quota
//	mergeClineCredentials       Cline credentials, tier and catalog
//
// UsageTotal and the daily token fields are absent from every step on purpose:
// they are gateway-owned atomic counters mutated only by IncrementAccountStats,
// so copying them from a snapshot would race the increment script and lose
// usage silently.
type accountPatch struct {
	updated  *Account
	acc      *Account
	existing *Account

	// staleSnapshot reports that acc was read before the stored row was last
	// written. A request may persist its verdict after a background quota
	// refresh has already written a newer value, so a stale snapshot may extend
	// a deadline but must never shorten or erase it.
	staleSnapshot bool
}

func (p accountPatch) mergeIdentity() {
	u, acc, existing := p.updated, p.acc, p.existing
	u.Name = acc.Name
	if acc.AccountType == "" {
		u.AccountType = existing.AccountType
	} else {
		u.AccountType = acc.AccountType
	}
	u.ClientCookie = acc.ClientCookie
	u.RefreshToken = acc.RefreshToken
	u.UserID = acc.UserID
	u.AgentMode = acc.AgentMode
	u.Email = acc.Email
	u.Weight = acc.Weight
	u.MaxConcurrent = acc.MaxConcurrent
	u.Enabled = acc.Enabled
	if strings.EqualFold(u.AccountType, "grok") && acc.QualityRevision < existing.QualityRevision {
		// A stale request cannot re-enable a credential disabled by a newer
		// atomic quality verdict. A current admin snapshot can still enable it.
		u.Enabled = existing.Enabled
	}
	u.Token = acc.Token
	u.Subscription = acc.Subscription
}

func (p accountPatch) mergeUsageMeter() {
	u, acc, existing := p.updated, p.acc, p.existing
	// WorkBuddy's generic usage slots mirror its credit-meter snapshot, so they
	// follow the same rule as the snapshot itself: only a write carrying a meter
	// reading at least as new as the stored one may move them. Any other write —
	// the login flow updating an existing row in place, an edit that never read
	// the meter, a copy cached before a sync — otherwise erases the number the
	// operator reads while the snapshot survives, which is how a live account
	// came to report "0 of 0" beside "350 remaining".
	if !strings.EqualFold(acc.AccountType, "workbuddy") || workBuddyMeterReadingIsNewer(acc, existing) {
		u.UsageCurrent = acc.UsageCurrent
		u.UsageLimit = acc.UsageLimit
	}
}

func (p accountPatch) mergeStatusAndVerdict() {
	u, acc := p.updated, p.acc
	u.StatusCode = acc.StatusCode
	u.AuthStatus = acc.AuthStatus
	if acc.ClearVerifiedAt {
		u.AuthStatus = AccountAuthStatusActive
	}
	u.RateLimitFailures = acc.RateLimitFailures
	// The reason describes the CURRENT status only. It must never outlive the
	// status it explains, or a recovered account keeps showing a stale error.
	if strings.TrimSpace(acc.StatusCode) == "" {
		u.StatusMessage = ""
	} else {
		u.StatusMessage = acc.StatusMessage
	}
	u.LastAttempt = acc.LastAttempt
	// A verdict timestamp is monotonic per credential: an unrelated partial
	// update (request counters, quota rotation) must not un-verify an account.
	// Replacing a credential clears it explicitly via ClearVerifiedAt.
	switch {
	case acc.ClearVerifiedAt:
		u.VerifiedAt = time.Time{}
	case !acc.VerifiedAt.IsZero():
		u.VerifiedAt = acc.VerifiedAt
	}
	u.ClearVerifiedAt = false
	// A current snapshot may clear the reset deadline; a stale one may only
	// extend it (see staleSnapshot).
	switch {
	case acc.QuotaResetAt.After(u.QuotaResetAt):
		u.QuotaResetAt = acc.QuotaResetAt
	case !p.staleSnapshot:
		u.QuotaResetAt = acc.QuotaResetAt
	}
}

func (p accountPatch) mergeGrokCredentials() {
	u, acc, existing := p.updated, p.acc, p.existing
	// Grok Build CLI OAuth credentials and identity must survive refresh /
	// admin updates. Leaving these out would silently drop rotated tokens.
	if strings.TrimSpace(acc.CredentialType) == "" {
		u.CredentialType = existing.CredentialType
	} else {
		u.CredentialType = acc.CredentialType
	}
	u.OAuthAccessToken = acc.OAuthAccessToken
	u.OAuthRefreshToken = acc.OAuthRefreshToken
	u.OAuthExpiresAt = acc.OAuthExpiresAt
	if strings.TrimSpace(acc.TeamID) == "" {
		u.TeamID = existing.TeamID
	} else {
		u.TeamID = acc.TeamID
	}
	if strings.TrimSpace(acc.GrokProvider) == "" {
		u.GrokProvider = existing.GrokProvider
	} else {
		u.GrokProvider = acc.GrokProvider
	}
	// Account updates are often partial (for example request counters and
	// credential rotation). Provider snapshots are refreshed independently, so
	// never erase a successfully observed catalog/billing window with a zero
	// value from an unrelated update.
	modelsNewer := acc.GrokModelsSyncedAt.After(existing.GrokModelsSyncedAt)
	if acc.GrokModels != nil && (!p.staleSnapshot || modelsNewer) {
		u.GrokModels = append([]string(nil), acc.GrokModels...)
	}
	if acc.GrokModelCatalog != nil && (!p.staleSnapshot || modelsNewer) {
		u.GrokModelCatalog = modelcatalog.CloneProfiles(acc.GrokModelCatalog)
	}
	if !acc.GrokModelsSyncedAt.IsZero() && (!p.staleSnapshot || modelsNewer) {
		u.GrokModelsSyncedAt = acc.GrokModelsSyncedAt
	}
	if !acc.GrokBilling.SyncedAt.IsZero() {
		u.GrokBilling = acc.GrokBilling
	}
	if !acc.GrokRateLimits.ObservedAt.IsZero() {
		u.GrokRateLimits = acc.GrokRateLimits
	}
	if !acc.GrokFreeQuota.ConfirmedAt.IsZero() {
		u.GrokFreeQuota = acc.GrokFreeQuota
	}
}

func (p accountPatch) mergeModelCooldownState() {
	u, acc, existing := p.updated, p.acc, p.existing
	// Per-model cooldowns are merged rather than replaced: an update written by a
	// path that did not touch them (a request counter, a quota refresh) must not
	// drop a cooldown another path just recorded.
	u.ModelCooldowns = mergeModelCooldowns(existing.ModelCooldowns, acc.ModelCooldowns)
	// The labels move with the deadlines they describe, or the pool would
	// report the previous verdict for a model that was just re-judged.
	u.ModelCooldownReasons = mergeModelCooldownReasons(existing.ModelCooldownReasons, acc.ModelCooldownReasons, existing.ModelCooldowns, acc.ModelCooldowns)
}

func (p accountPatch) mergeWorkBuddyCredentials() {
	u, acc, existing := p.updated, p.acc, p.existing
	// WorkBuddy credentials are rotated by the upstream (Keycloak rotates the
	// refresh token on every renewal) and account updates are frequently
	// partial, so an empty value means "keep what is stored", never "erase".
	if acc.ReplaceWorkBuddyCredentials {
		u.WorkBuddyAccessToken = strings.TrimSpace(acc.WorkBuddyAccessToken)
		u.WorkBuddyRefreshToken = strings.TrimSpace(acc.WorkBuddyRefreshToken)
		u.WorkBuddyExpiresAt = acc.WorkBuddyExpiresAt
	}
	patchString(&u.WorkBuddyUID, acc.WorkBuddyUID)
	if len(acc.WorkBuddyModelIDs) > 0 {
		u.WorkBuddyModelIDs = append([]string(nil), acc.WorkBuddyModelIDs...)
	}
	if !acc.WorkBuddyModelsSyncedAt.IsZero() {
		u.WorkBuddyModelsSyncedAt = acc.WorkBuddyModelsSyncedAt
	}
	if workBuddyMeterReadingIsNewer(acc, existing) {
		u.WorkBuddyQuota = acc.WorkBuddyQuota
	}
}

func (p accountPatch) mergeQoderCredentials() {
	u, acc, existing := p.updated, p.acc, p.existing
	// Qoder credentials are rotated by the upstream and account updates are
	// frequently partial (a request counter, a quota refresh), so an empty value
	// means "keep what is stored", never "erase". The derived runtime pair is
	// written once at login and then reused, which is why it follows the same
	// keep-on-empty rule instead of being regenerated per request.
	if acc.ReplaceQoderCredentials {
		u.QoderAccessToken = strings.TrimSpace(acc.QoderAccessToken)
		u.QoderRefreshToken = strings.TrimSpace(acc.QoderRefreshToken)
		u.QoderExpiresAt = acc.QoderExpiresAt
		u.QoderMachineID = strings.TrimSpace(acc.QoderMachineID)
		u.QoderRuntimeInfo = strings.TrimSpace(acc.QoderRuntimeInfo)
		u.QoderRuntimeKey = strings.TrimSpace(acc.QoderRuntimeKey)
		u.QoderRuntimeIdentity = strings.TrimSpace(acc.QoderRuntimeIdentity)
	}
	patchString(&u.QoderUserID, acc.QoderUserID)
	patchString(&u.QoderUserName, acc.QoderUserName)
	patchString(&u.QoderOrganizationID, acc.QoderOrganizationID)
	if len(acc.QoderOrganizationTags) > 0 {
		u.QoderOrganizationTags = append([]string(nil), acc.QoderOrganizationTags...)
	}
	if acc.QoderDataPolicy {
		u.QoderDataPolicy = true
	}
	if len(acc.QoderModelIDs) > 0 {
		u.QoderModelIDs = append([]string(nil), acc.QoderModelIDs...)
	}
	if !acc.QoderModelsSyncedAt.IsZero() && (existing.QoderModelsSyncedAt.IsZero() || !acc.QoderModelsSyncedAt.Before(existing.QoderModelsSyncedAt)) {
		u.QoderModelsSyncedAt = acc.QoderModelsSyncedAt
	}
	if !acc.QoderQuota.SyncedAt.IsZero() && (existing.QoderQuota.SyncedAt.IsZero() || !acc.QoderQuota.SyncedAt.Before(existing.QoderQuota.SyncedAt)) {
		u.QoderQuota = acc.QoderQuota
	}
}

func (p accountPatch) mergeClineCredentials() {
	u, acc, existing := p.updated, p.acc, p.existing
	// Cline credentials are rotated by the upstream on every refresh, and
	// account updates are frequently partial, so an empty value means "keep
	// what is stored", never "erase". Only an explicit replace intent writes
	// a new pair, so a snapshot read before a rotation cannot rewind it.
	if acc.ReplaceClineCredentials {
		u.ClineAccessToken = strings.TrimSpace(acc.ClineAccessToken)
		u.ClineRefreshToken = strings.TrimSpace(acc.ClineRefreshToken)
		u.ClineExpiresAt = acc.ClineExpiresAt
	}
	patchString(&u.ClineEmail, acc.ClineEmail)
	// The tier follows the same rule as the credentials: an account update
	// is frequently partial, so an empty value means "keep what is stored".
	// That matters because "no tier recorded" and "tier is free" are
	// different states, and only one of them is evidence.
	patchString(&u.ClinePlan, acc.ClinePlan)
	if len(acc.ClineModelIDs) > 0 {
		u.ClineModelIDs = append([]string(nil), acc.ClineModelIDs...)
	}
	if !acc.ClineModelsSyncedAt.IsZero() && (existing.ClineModelsSyncedAt.IsZero() || !acc.ClineModelsSyncedAt.Before(existing.ClineModelsSyncedAt)) {
		u.ClineModelsSyncedAt = acc.ClineModelsSyncedAt
	}
}
