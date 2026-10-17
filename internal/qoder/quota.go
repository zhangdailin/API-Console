package qoder

import (
	"context"
	"fmt"
	"net/http"
	"orchids-api/internal/httpclient"
	"strings"
	"sync"
	"time"

	"encoding/json"

	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// The chat path is not the only place an account's allowance is visible. The
// gateway answers two control-plane reads that the reference implementation never
// used but the official CLI calls on login and on status display:
//
//	GET /api/v2/user/plan    plan tier, paid flag, feature switches, start date
//	GET /api/v2/quota/usage  the credit window: total, used, remaining, reset
//
// They are what turns "the account failed" into "the account has no credits left,
// here is the upgrade link", which is the difference between an operator guessing
// at a credential problem and knowing it is a billing state.

// Quota is one account's credit state as the gateway reports it.
type Quota struct {
	PlanQuota  store.QoderCreditPool
	AddOnQuota store.QoderCreditPool
	OrgQuota   store.QoderCreditPool
	// PlanTier is the plan label, for example "Free".
	PlanTier string
	// UserType is the account class, for example "personal_standard".
	UserType string
	// PaidPlan reports whether the account is on a paid tier.
	PaidPlan bool
	// Limit, Used and Remaining describe the credit window. The gateway reports
	// them in credits.
	Limit     float64
	Used      float64
	Remaining float64
	// Exhausted is the gateway's own verdict. It is authoritative over the
	// arithmetic above: an account whose numbers have not refreshed yet can still
	// be flagged exhausted.
	Exhausted bool
	// Unit is the upstream unit, normally "credits".
	Unit string
	// PerModelCap is the percentage cap one model may consume of the window, when
	// the plan reports one.
	PerModelCap float64
	// ResetAt is the next allowance window boundary; PeriodEnd is when the quota
	// itself lapses.
	ResetAt   time.Time
	PeriodEnd time.Time
	// UpgradeURL is the page the gateway points an exhausted account at.
	UpgradeURL string
	// SyncedAt marks when this snapshot was observed.
	SyncedAt time.Time
}

// quotaUsageResponse is GET /api/v2/quota/usage.
type quotaUsageResponse struct {
	UserID          string  `json:"userId"`
	UserType        string  `json:"userType"`
	TotalUsagePct   float64 `json:"totalUsagePercentage"`
	IsQuotaExceeded bool    `json:"isQuotaExceeded"`
	ExpiresAt       int64   `json:"expiresAt"`
	LimitExceeded   bool    `json:"limitExceeded"`
	UpgradeURL      string  `json:"upgradeUrl"`
	ModelUsage      []struct {
		UsagePercentage  float64 `json:"usagePercentage"`
		TotalUsagePct    float64 `json:"totalUsagePercentage"`
		PerModelQuotaPct float64 `json:"perModelQuotaPercentage"`
	} `json:"modelUsage"`
	UserQuota          *quotaPoolResponse `json:"userQuota"`
	AddOnQuota         *quotaPoolResponse `json:"addOnQuota"`
	OrgResourcePackage *quotaPoolResponse `json:"orgResourcePackage"`
}

type quotaPoolResponse struct {
	Total     float64 `json:"total"`
	Cap       float64 `json:"cap"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	Unit      string  `json:"unit"`
	Available bool    `json:"available"`
}

// planResponse is GET /api/v2/user/plan.
type planResponse struct {
	UserType     string `json:"user_type"`
	PlanTierName string `json:"plan_tier_name"`
	IsPaidPlan   bool   `json:"is_paid_plan"`
}

// FetchQuota reads the account's credit state.
//
// A missing quota is not an error the caller must fail on: the usage endpoint is
// a separate read, and a deployment that cannot reach it should still serve chat.
// The returned error is therefore informative rather than fatal. Usage must be
// observed successfully before a snapshot is returned: plan/status metadata alone
// cannot establish counters or exhaustion. On error callers must preserve the
// previous snapshot and exhaustion state. Plan/status reads are optional enrichment.
func (c *Client) FetchQuota(ctx context.Context) (*Quota, error) {
	if c == nil {
		return nil, fmt.Errorf("qoder client is nil")
	}
	creds, err := c.ensureAccessToken(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	quota := &Quota{SyncedAt: now}

	usage, usageErr := c.getUsage(ctx, creds)
	if usageErr != nil {
		return nil, usageErr
	}
	quota.UserType = strings.TrimSpace(usage.UserType)
	quota.Exhausted = usage.IsQuotaExceeded || usage.LimitExceeded
	quota.UpgradeURL = strings.TrimSpace(usage.UpgradeURL)
	quota.PlanQuota = observedCreditPool(usage.UserQuota, false)
	quota.AddOnQuota = observedCreditPool(usage.AddOnQuota, false)
	quota.OrgQuota = observedCreditPool(usage.OrgResourcePackage, true)
	for _, pool := range []store.QoderCreditPool{quota.PlanQuota, quota.AddOnQuota, quota.OrgQuota} {
		if !pool.Known || !pool.Available || pool.Limit <= 0 {
			continue
		}
		quota.Limit += pool.Limit
		quota.Used += pool.Used
		quota.Remaining += pool.Remaining
		if quota.Unit == "" {
			quota.Unit = pool.Unit
		}
	}
	quota.Unit = util.FirstNonEmpty(quota.Unit, "credits")
	if usage.ExpiresAt > 0 {
		// normalizeMillis already yields seconds; unixSeconds must not
		// normalize a second time, or a far-future expiry wraps into 1978.
		quota.PeriodEnd = time.Unix(normalizeMillis(usage.ExpiresAt), 0)
	}
	quota.PerModelCap = highestModelQuotaShare(usage)
	// Only the complete spendable balance can establish numeric exhaustion.
	// An explicit upstream exhaustion verdict still wins over positive counters.
	if quota.Limit > 0 && quota.Remaining <= 0 {
		quota.Exhausted = true
	}

	// Metadata reads are independent and optional. Fetch both concurrently: a
	// successful plan read must never suppress the reported reset boundary, and
	// one slow metadata service must not delay the other by its full timeout.
	var plan *planResponse
	var status *statusResponse
	var metadata sync.WaitGroup
	metadata.Go(func() { plan, _ = c.getPlan(ctx, creds) })
	metadata.Go(func() { status, _ = c.getStatus(ctx, creds) })
	metadata.Wait()
	if plan != nil {
		quota.PlanTier = strings.TrimSpace(plan.PlanTierName)
		quota.PaidPlan = plan.IsPaidPlan
		if quota.UserType == "" {
			quota.UserType = strings.TrimSpace(plan.UserType)
		}
	}
	if status != nil {
		quota.PlanTier = util.FirstNonEmpty(quota.PlanTier, status.UserTag, status.Plan)
		quota.UserType = util.FirstNonEmpty(quota.UserType, status.UserType)
		quota.ResetAt = reportedQuotaReset(status.NextResetAt)
	}

	return quota, nil
}

func observedCreditPool(raw *quotaPoolResponse, organization bool) store.QoderCreditPool {
	if raw == nil {
		return store.QoderCreditPool{}
	}
	limit, available := raw.Total, true
	if organization {
		limit, available = raw.Cap, raw.Available
	}
	return store.QoderCreditPool{Known: true, Available: available, Limit: limit,
		Used: raw.Used, Remaining: raw.Remaining, Unit: util.FirstNonEmpty(raw.Unit, "credits")}
}

// A subscription start or expiry is not a reset schedule. Only a reported
// boundary is eligible; the upstream's year-9999 sentinel means no boundary.
func reportedQuotaReset(value int64) time.Time {
	// Year-9999 seconds exceed normalizeMillis' modern-epoch heuristic, so
	// reject that range before deciding whether the value is milliseconds.
	if value <= 0 || (value >= 253370764800 && value <= 253402300799) {
		return time.Time{}
	}
	reset := time.Unix(normalizeMillis(value), 0)
	if reset.Year() >= 9999 {
		return time.Time{}
	}
	return reset
}

// statusResponse is GET /api/v3/user/status.
type statusResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	UserType    string `json:"userType"`
	UserTag     string `json:"userTag"`
	Plan        string `json:"plan"`
	NextResetAt int64  `json:"nextResetAt"`
}

func (c *Client) getUsage(ctx context.Context, creds Credentials) (*quotaUsageResponse, error) {
	var out quotaUsageResponse
	if err := c.getJSON(ctx, c.endpoints.openAPI+"/api/v2/quota/usage", creds, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) getPlan(ctx context.Context, creds Credentials) (*planResponse, error) {
	var out planResponse
	if err := c.getJSON(ctx, c.endpoints.openAPI+"/api/v2/user/plan", creds, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) getStatus(ctx context.Context, creds Credentials) (*statusResponse, error) {
	var out statusResponse
	if err := c.getJSON(ctx, c.endpoints.openAPI+"/api/v3/user/status", creds, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// getJSON performs one authenticated OpenAPI read. These endpoints take the
// device access token as a plain Bearer; they are not COSY-signed, which is why
// they are useful for diagnosing an account whose signed chat request failed.
func (c *Client) getJSON(ctx context.Context, url string, creds Credentials, out any) error {
	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(creds.AccessToken))
	req.Header.Set("User-Agent", userAgent(c.clientVersion))
	if machine := strings.TrimSpace(c.machineTokenOr(c.machineID)); machine != "" {
		// The CLI sends the device token alongside the bearer on this surface.
		// It is the derived token, so this control-plane call presents the same
		// device the signed chat request does.
		req.Header.Set("Cosy-MachineToken", machine)
	}

	resp, raw, err := httpclient.DoReadBody(c.control, req, 64<<10)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return apiError(http.MethodGet, url, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", signPath(url), err)
	}
	return nil
}

// normalizeMillis folds a millisecond timestamp to seconds. The OpenAPI surface
// answers milliseconds while the COSY surface answers seconds.
func normalizeMillis(value int64) int64 {
	if value > 1e11 {
		return value / 1000
	}
	return value
}

// highestModelQuotaShare reports the largest per-model cap the plan declares, as
// a fraction. The gateway states it as a percentage.
func highestModelQuotaShare(usage *quotaUsageResponse) float64 {
	highest := 0.0
	for _, model := range usage.ModelUsage {
		for _, candidate := range []float64{model.PerModelQuotaPct, model.UsagePercentage, model.TotalUsagePct} {
			if candidate > highest && candidate <= 1 {
				highest = candidate
			}
		}
	}
	return highest
}

// ApplyQuota folds a quota snapshot onto the account's scheduling fields.
//
// UsageCurrent carries what is LEFT, which is the convention the other channels
// use for a cycle allowance and what the console reads as "remaining".
func ApplyQuota(acc *store.Account, quota *Quota) {
	if acc == nil || quota == nil {
		return
	}
	previous := acc.QoderQuota
	acc.UsageLimit = quota.Limit
	acc.UsageCurrent = quota.Remaining
	acc.QoderQuota = store.QoderQuotaSnapshot{
		PlanQuota:      quota.PlanQuota,
		AddOnQuota:     quota.AddOnQuota,
		OrgQuota:       quota.OrgQuota,
		Limit:          quota.Limit,
		Remaining:      quota.Remaining,
		Used:           quota.Used,
		Exhausted:      quota.Exhausted,
		PlanTier:       quota.PlanTier,
		UserType:       quota.UserType,
		PaidPlan:       quota.PaidPlan,
		Unit:           quota.Unit,
		UpgradeURL:     quota.UpgradeURL,
		ResetAt:        quota.ResetAt,
		PeriodEnd:      quota.PeriodEnd,
		LastKnownLimit: util.FirstPositive(quota.Limit, previous.Limit),
		SyncedAt:       quota.SyncedAt,
	}
	// A fresh observation without a boundary must also retire an old inferred
	// reset; otherwise generic consumers keep displaying an invented deadline.
	acc.QuotaResetAt = quota.ResetAt
}
