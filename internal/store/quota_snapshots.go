package store

import (
	"time"
)

// QoderQuotaSnapshot is one Qoder credit/plan observation.
//
// Exhausted is the gateway's own verdict and is authoritative over the
// arithmetic: an account whose counters have not refreshed can still be flagged
// spent, which is what makes it usable as a scheduling signal.
type QoderQuotaSnapshot struct {
	Limit          float64   `json:"limit,omitempty"`
	Remaining      float64   `json:"remaining,omitempty"`
	Used           float64   `json:"used,omitempty"`
	Exhausted      bool      `json:"exhausted,omitempty"`
	PlanTier       string    `json:"plan_tier,omitempty"`
	UserType       string    `json:"user_type,omitempty"`
	PaidPlan       bool      `json:"paid_plan,omitempty"`
	Unit           string    `json:"unit,omitempty"`
	UpgradeURL     string    `json:"upgrade_url,omitempty"`
	ResetAt        time.Time `json:"reset_at"`
	PeriodEnd      time.Time `json:"period_end"`
	LastKnownLimit float64   `json:"last_known_limit,omitempty"`
	SyncedAt       time.Time `json:"synced_at"`
}

// ResyncAt reports when the snapshot should be refreshed again. A quota that is
// spent is the interesting case: the reset is the only moment it can recover, so
// the snapshot is worth re-reading then.
func (s QoderQuotaSnapshot) ResyncAt() time.Time {
	if s.SyncedAt.IsZero() {
		return time.Time{}
	}
	if !s.ResetAt.IsZero() {
		return s.ResetAt
	}
	return s.SyncedAt
}

// WorkBuddyQuotaSnapshot is the WorkBuddy credit-meter snapshot. Remaining/Limit
// describe the current cycle; Used/LastConsumedUnits are whole-credit figures
// derived from the meter, because the upstream also reports fractions.
type WorkBuddyQuotaSnapshot struct {
	Limit             float64   `json:"limit,omitempty"`
	Remaining         float64   `json:"remaining,omitempty"`
	Used              float64   `json:"used,omitempty"`
	PackageRemaining  float64   `json:"package_remaining,omitempty"`
	LastConsumedUnits int       `json:"last_consumed_units,omitempty"`
	ResetAt           time.Time `json:"reset_at"`
	PeriodEnd         time.Time `json:"period_end"`
	PackageName       string    `json:"package_name,omitempty"`
	Unit              string    `json:"unit,omitempty"`
	SyncedAt          time.Time `json:"synced_at"`
}

// ResyncAt reports when the quota snapshot needs refreshing. The cycle reset is
// the hard deadline: the allowance is re-armed then, but the console also wants
// the displayed number to stay current between resets.
func (s WorkBuddyQuotaSnapshot) ResyncAt() time.Time {
	if s.SyncedAt.IsZero() {
		return time.Time{}
	}
	if s.ResetAt.IsZero() {
		return s.SyncedAt
	}
	return s.ResetAt
}

// GrokQuotaWindow is one explicit upstream usage or throttling dimension.
// Values are meaningful only when their Has* marker is true; zero is valid.
type GrokQuotaWindow struct {
	Limit        float64   `json:"limit,omitempty"`
	Remaining    float64   `json:"remaining,omitempty"`
	UsagePercent float64   `json:"usage_percent,omitempty"`
	HasLimit     bool      `json:"has_limit,omitempty"`
	HasRemaining bool      `json:"has_remaining,omitempty"`
	HasUsage     bool      `json:"has_usage,omitempty"`
	ResetAt      time.Time `json:"reset_at"`
}

// GrokBillingSnapshot stores official Build weekly/monthly windows only.
type GrokBillingSnapshot struct {
	Weekly   GrokQuotaWindow `json:"weekly"`
	Monthly  GrokQuotaWindow `json:"monthly"`
	SyncedAt time.Time       `json:"synced_at"`
	Source   string          `json:"source,omitempty"`
	// NextProbeAt serializes probes after an exhausted paid period ends. Before
	// the first claim PeriodEnd is the due time; each claim advances this by the
	// bounded retry interval so concurrent selectors cannot hammer billing.
	NextProbeAt time.Time `json:"next_probe_at"`
	LastProbeAt time.Time `json:"last_probe_at"`
}

const GrokPaidQuotaProbeInterval = 15 * time.Minute

// IsExhausted reports an authoritative paid-billing exhaustion signal. Monthly
// numeric allowance wins when present; otherwise a 100% weekly usage snapshot
// is sufficient when it also carries a real billing period.
func (b GrokBillingSnapshot) IsExhausted() bool {
	if b.Monthly.HasLimit && b.Monthly.Limit > 0 && b.Monthly.HasRemaining && b.Monthly.Remaining <= 0 {
		return true
	}
	return b.Weekly.HasUsage && b.Weekly.UsagePercent >= 100 && !b.Weekly.ResetAt.IsZero()
}

// PeriodEnd returns the latest known paid billing reset.
func (b GrokBillingSnapshot) PeriodEnd() time.Time {
	if b.Monthly.ResetAt.After(b.Weekly.ResetAt) {
		return b.Monthly.ResetAt
	}
	return b.Weekly.ResetAt
}

// GrokRateLimitSnapshot stores passive response headers separately from
// billing. They can be useful for cooldown and diagnostics but must never be
// rendered as a paid-plan balance.
type GrokRateLimitSnapshot struct {
	Requests   GrokQuotaWindow `json:"requests"`
	Tokens     GrokQuotaWindow `json:"tokens"`
	Model      string          `json:"model,omitempty"`
	ObservedAt time.Time       `json:"observed_at"`
}

// GrokFreeQuotaSnapshot is the Free allowance window the upstream CONFIRMED by
// refusing a request ("subscription:free-usage-exhausted ... tokens (actual/limit):
// N/M"). It is the one place a Free limit becomes a fact rather than an estimate, so
// it is kept apart from GrokBilling (a paid window this account never returned) and
// from the estimate derived from an inferred Free profile.
type GrokFreeQuotaSnapshot struct {
	Used        float64   `json:"used,omitempty"`
	Limit       float64   `json:"limit,omitempty"`
	HasLimit    bool      `json:"has_limit,omitempty"`
	ResetAt     time.Time `json:"reset_at"`
	ConfirmedAt time.Time `json:"confirmed_at"`
}
