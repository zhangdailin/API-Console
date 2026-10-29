package workbuddy

import (
	"context"
	"fmt"
	"net/http"
	"orchids-api/internal/httpclient"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// The WorkBuddy backend meters credits through the shared Tencent Cloud AI Code
// Assistant meter (`p_tcaca`). Two windows are reported per package:
//
//   - the package window  (Capacity*/CycleEndTime of the deal)
//   - the current cycle   (CycleCapacity*, refreshed on the cycle boundary)
//
// The remaining cycle capacity is what an operator acts on, so it maps to the
// generic UsageCurrent slot (which this channel documents as "remaining"), while
// the package size is the ceiling for the current cycle.
const (
	billingProductCode = "p_tcaca"
	// billingPageSize keeps one page big enough for every personal package.
	billingPageSize = 100
	// billingWindowDays bounds the package end-time filter.
	billingWindowDays = 365 * 101
)

// Quota is the account's credit state as reported by the meter.
type Quota struct {
	// Remaining and Limit describe the current cycle.
	Remaining float64
	Limit     float64
	// ResetAt is the next cycle boundary.
	ResetAt time.Time
	// PeriodEnd is when the package itself expires.
	PeriodEnd time.Time
	// PackageName is the upstream plan label, e.g. "Bonus Pack".
	PackageName string
	// PackageRemaining is what is left of the whole package.
	PackageRemaining float64
	// Unit is the upstream capacity unit, normally "credit".
	Unit string
	// SyncedAt marks when this snapshot was observed.
	SyncedAt time.Time
}

// meterAccount is one metered package in the billing response.
type meterAccount struct {
	PackageName           string  `json:"PackageName"`
	CapacityUnit          string  `json:"CapacityUnit"`
	CapacitySize          float64 `json:"CapacitySize"`
	CapacityRemain        float64 `json:"CapacityRemain"`
	CapacitySizePrecise   string  `json:"CapacitySizePrecise"`
	CapacityRemainPrecise string  `json:"CapacityRemainPrecise"`
	CycleCapacitySize     float64 `json:"CycleCapacitySize"`
	CycleCapacityRemain   float64 `json:"CycleCapacityRemain"`
	CycleCapacitySizeP    string  `json:"CycleCapacitySizePrecise"`
	CycleCapacityRemainP  string  `json:"CycleCapacityRemainPrecise"`
	CycleCapacityUsedP    string  `json:"CycleCapacityUsedPrecise"`
	CycleEndTime          string  `json:"CycleEndTime"`
	ExpiredTime           string  `json:"ExpiredTime"`
	Status                int     `json:"Status"`
}

type resourceResponse struct {
	Response struct {
		Data struct {
			TotalCount int            `json:"TotalCount"`
			Accounts   []meterAccount `json:"Accounts"`
		} `json:"Data"`
	} `json:"Response"`
}

// FetchQuota reads the account's credit allowance. A missing package is not an
// error: it reports a zero allowance so the UI can say "no credits" instead of
// inventing a value.
func (c *Client) FetchQuota(ctx context.Context) (quota *Quota, fetchErr error) {
	defer func() { c.persistRestriction(ctx, fetchErr) }()
	if c == nil {
		return nil, fmt.Errorf("workbuddy client is nil")
	}
	accessToken, err := c.ensureAccessToken(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	body, err := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 billingPageSize,
		"ProductCode":              billingProductCode,
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(billingWindowDays * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v2/billing/meter/get-user-resource", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	applyHeaders(req, accessToken, c.creds.UID, "application/json")
	req.Header.Set("User-Agent", billingUA)

	resp, raw, err := httpclient.DoReadBody(c.httpClient, req, 8<<20)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch workbuddy credits: %w", err)
	}
	data, err := unwrapEnvelope(resp.StatusCode, raw)
	if err != nil {
		return nil, err
	}

	var payload resourceResponse
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("failed to decode workbuddy credits: %w", err)
	}
	return summarizeQuota(payload, now), nil
}

// summarizeQuota aggregates every metered package into one allowance window.
func summarizeQuota(payload resourceResponse, now time.Time) *Quota {
	quota := &Quota{SyncedAt: now, Unit: "credit"}
	// The package label describes the package the allowance mostly comes from, so
	// it follows the largest remaining balance rather than whichever row the
	// upstream happened to append last.
	labelRemain := -1.0
	for _, account := range payload.Response.Data.Accounts {
		cycleRemain := preciseOr(account.CycleCapacityRemainP, account.CycleCapacityRemain)
		cycleSize := util.FirstPositive(
			parsePrecise(account.CycleCapacitySizeP),
			account.CycleCapacitySize,
			account.CapacitySize,
		)
		packageRemain := preciseOr(account.CapacityRemainPrecise, account.CapacityRemain, cycleRemain)
		if cycleSize <= 0 && cycleRemain <= 0 && packageRemain <= 0 {
			continue
		}

		quota.Remaining += cycleRemain
		quota.Limit += cycleSize
		quota.PackageRemaining += packageRemain
		if name := strings.TrimSpace(account.PackageName); name != "" && cycleRemain >= labelRemain {
			quota.PackageName = name
			if unit := strings.TrimSpace(account.CapacityUnit); unit != "" {
				quota.Unit = unit
			}
			labelRemain = cycleRemain
		}
		if resetAt := parseMeterTime(account.CycleEndTime); !resetAt.IsZero() {
			if quota.ResetAt.IsZero() || resetAt.Before(quota.ResetAt) {
				quota.ResetAt = resetAt
			}
		}
		if periodEnd := parseMeterTime(account.ExpiredTime); !periodEnd.IsZero() {
			if quota.PeriodEnd.IsZero() || periodEnd.After(quota.PeriodEnd) {
				quota.PeriodEnd = periodEnd
			}
		}
	}

	if quota.Limit > 0 && quota.Remaining > quota.Limit {
		quota.Remaining = quota.Limit
	}
	return quota
}

// preciseNumberReplacer strips the grouping a large allowance may carry: the
// meter reports "1,234.00" for four-digit packages.
var preciseNumberReplacer = strings.NewReplacer(",", "", " ", "", "\u00a0", "")

func parsePrecise(raw string) float64 {
	raw = preciseNumberReplacer.Replace(strings.TrimSpace(raw))
	if raw == "" {
		return 0
	}
	var value float64
	if _, err := fmt.Sscanf(raw, "%f", &value); err != nil {
		return 0
	}
	return value
}

// preciseOr reports the precise field's value when the upstream actually sent
// one, even when that value is zero, and falls back to the coarse fields only
// when it did not. A genuine "0.00" remaining is a reading: treating it as
// "absent" (which util.FirstPositive does) let a stale coarse value win and made
// a spent account look funded.
func preciseOr(raw string, fallbacks ...float64) float64 {
	if strings.TrimSpace(raw) != "" {
		return parsePrecise(raw)
	}
	return util.FirstPositive(fallbacks...)
}

// parseMeterTime reads the upstream "2006-01-02 15:04:05" wall-clock format.
// Empty or placeholder values ("9999-99-99 ...") mean no expiry.
//
// The upstream never says which zone that clock is in, and nothing in the payload
// pins it: the same "2026-09-30 23:59:59" window end came back for two days
// across accounts synced at different times, so it is a fixed window end rather
// than a local midnight that would give the zone away. It is therefore read as
// UTC, and consumers treat the result as an order-of-magnitude hint rather than a
// deadline: the free-tier hold is capped, and the background refresh runs on an
// elapsed cadence. Settling the convention needs one observation — comparing this
// value with the moment the meter actually re-arms the allowance.
func parseMeterTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "9999") {
		return time.Time{}
	}
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if parsed, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// ApplyQuota copies a quota snapshot onto the account record. UsageCurrent holds
// the remaining credits for this channel, matching every other provider the
// account table renders.
func ApplyQuota(acc *store.Account, quota *Quota) {
	if acc == nil || quota == nil {
		return
	}
	previous := acc.WorkBuddyQuota
	acc.UsageLimit = quota.Limit
	acc.UsageCurrent = quota.Remaining
	acc.WorkBuddyQuota = store.WorkBuddyQuotaSnapshot{
		Limit:             quota.Limit,
		Remaining:         quota.Remaining,
		Used:              quota.VisibleUsed(),
		PackageRemaining:  quota.PackageRemaining,
		ResetAt:           quota.ResetAt,
		PeriodEnd:         quota.PeriodEnd,
		PackageName:       quota.PackageName,
		Unit:              quota.Unit,
		LastConsumedUnits: quota.LastConsumedUnits(previous),
		SyncedAt:          quota.SyncedAt,
	}
	if !quota.ResetAt.IsZero() {
		acc.QuotaResetAt = quota.ResetAt
	}
}

// VisibleUsed returns the credit consumption the operator can see: whole credits
// removed from the cycle allowance (the upstream also counts fractional credits).
func (q *Quota) VisibleUsed() float64 {
	if q == nil || q.Limit <= 0 {
		return 0
	}
	used := q.Limit - q.Remaining
	if used < 0 {
		return 0
	}
	return used
}

// LastConsumedUnits derives the whole credits consumed since the previous
// snapshot, which is what the account table's usage counter shows. A cycle
// boundary re-arms the allowance, so the counter resets instead of reporting a
// negative delta.
func (q *Quota) LastConsumedUnits(previous store.WorkBuddyQuotaSnapshot) int {
	if q == nil {
		return 0
	}
	used := int(q.VisibleUsed())
	if previous.SyncedAt.IsZero() || !previous.ResetAt.Equal(q.ResetAt) {
		return used
	}
	previousUsed := int(previous.Used)
	if used < previousUsed {
		return 0
	}
	return used - previousUsed
}
