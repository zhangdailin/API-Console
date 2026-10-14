package cline

import (
	"context"
	"fmt"
	"net/http"
	"orchids-api/internal/httpclient"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/util"
)

// PlanNoHistory is the answer the upstream gives for an account that never held
// a paid plan. It is a sentence rather than a code, and it is the only thing
// that distinguishes "free" from "we could not ask" — a subscriber's row is a
// plan name, so the two states have to be told apart by content.
const PlanNoHistory = "no plan history found for user"

// freePlanName records no active subscription, including an expired period.
// It never substitutes for a failed read; empty ClinePlan means not probed.
const freePlanName = "free"

// planRequestTimeout keeps a best-effort read from stalling a verification that
// has already proved the credential. Plan reads also gate the Pass catalog.
const planRequestTimeout = 15 * time.Second

// Plan reports the account's subscription tier as the upstream states it.
//
// The model feed cannot answer this. It publishes four tiers in one payload
// (recommended / free / clinePass / clineCloud) and the account's own
// entitlement is the thing that decides which of them actually serves, so "the
// free list is non-empty" proves free access and says nothing about a paid plan
// held alongside it. /users/me/plan is the endpoint that does: it names the
// plan for a subscriber and answers "no plan history found for user" for one
// that never subscribed.
//
// The distinction is verified rather than assumed: a pass-tier model answered
// 403 ENTITLEMENT_ERROR "the user is not subscribed to required model plan" for
// the account that gets the no-history answer here, while every free-tier model
// answered 200.
type Plan struct {
	// Name is the recorded tier: a plan name, or "free".
	Name string
	// Explicit reports whether the upstream answered at all. A transport
	// failure leaves both fields false so a caller never records a guess.
	Explicit bool
}

// fetchPlan reads the account's plan. A failure is returned as an error and
// never as a tier: an unreachable endpoint must not downgrade an account to
// "free" or invent a plan name.
func (c *Client) fetchPlan(ctx context.Context) (Plan, error) {
	if c == nil {
		return Plan{}, fmt.Errorf("cline client is nil")
	}
	creds, err := c.ensureAccessToken(ctx)
	if err != nil {
		return Plan{}, err
	}
	endpoint := c.apiBase + "/users/me/plan"

	reqCtx, cancel := context.WithTimeout(ctx, planRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Plan{}, fmt.Errorf("build cline plan request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+creds.Bearer())
	req.Header.Set("Accept", "application/json")
	applyClientHeaders(req.Header)

	resp, raw, err := httpclient.DoReadBody(c.control, req, 64<<10)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}

	var payload struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Data    *struct {
			Plan *struct {
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
			} `json:"plan"`
			CurrentPeriodEnd *time.Time `json:"currentPeriodEnd"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Plan{}, fmt.Errorf("decode cline plan response: %w", err)
	}

	// A subscriber: the endpoint returns a row and no error sentence.
	if resp.StatusCode == http.StatusOK {
		if !payload.Success || payload.Data == nil || payload.Data.Plan == nil {
			return Plan{}, fmt.Errorf("cline plan response carried no current subscription")
		}
		name := strings.TrimSpace(util.FirstNonEmpty(payload.Data.Plan.DisplayName, payload.Data.Plan.Name))
		if name == "" {
			// A 200 with nothing nameable is not evidence of a tier. Report it
			// as unreadable rather than as "free".
			return Plan{}, fmt.Errorf("cline plan response carried no plan name")
		}
		if end := payload.Data.CurrentPeriodEnd; end != nil && !end.After(time.Now()) {
			return Plan{Name: freePlanName, Explicit: true}, nil
		}
		return Plan{Name: name, Explicit: true}, nil
	}

	if resp.StatusCode == http.StatusNotFound && strings.Contains(strings.ToLower(payload.Error), PlanNoHistory) {
		return Plan{Name: freePlanName, Explicit: true}, nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Plan{}, fmt.Errorf("%w: %v", ErrCredentialMissing, apiError(http.MethodGet, endpoint, resp.StatusCode, raw))
	}
	return Plan{}, apiError(http.MethodGet, endpoint, resp.StatusCode, raw)
}

// FetchPlan is the provider-facing alias of fetchPlan.
func (c *Client) FetchPlan(ctx context.Context) (Plan, error) { return c.fetchPlan(ctx) }
