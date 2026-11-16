package warp

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"orchids-api/internal/store"
)

const quotaSource = "warp_request_limit_info"

// Request only anonymous quota measurements. Workspace windows must never be
// combined or described as an account's personal allowance.
const quotaQuery = `query GetRequestLimitInfo($requestContext: RequestContext!) { user(requestContext: $requestContext) { __typename ... on UserOutput { user { workspaces { includedUsageInfo { includedUsageCredits usageCreditsUsedSinceLastRefresh nextRefreshTime } } requestLimitInfo { isUnlimited requestsUsedSinceLastRefresh requestLimit nextRefreshTime } } } } }`

// quotaNumber accepts only finite, nonnegative JSON numbers. In particular,
// null, strings, overflow, and malformed values cannot become measured zeroes.
// Invalid fields do not prevent independent workspace observations being read.
type quotaNumber struct {
	value *float64
}

func (n *quotaNumber) UnmarshalJSON(raw []byte) error {
	n.value = nil
	if !quotaPresent(raw) {
		return nil
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return nil
	}
	if value == 0 {
		value = 0 // Canonicalize negative zero.
	}
	n.value = &value
	return nil
}

func quotaPresent(raw json.RawMessage) bool {
	text := strings.TrimSpace(string(raw))
	return text != "" && text != "null"
}

func quotaReset(raw json.RawMessage) (time.Time, bool) {
	var text string
	if !quotaPresent(raw) || json.Unmarshal(raw, &text) != nil {
		return time.Time{}, false
	}
	reset, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || reset.IsZero() {
		return time.Time{}, false
	}
	return reset.UTC(), true
}

func quotaRemaining(limit, used *float64) *float64 {
	if limit == nil || used == nil {
		return nil
	}
	remaining := math.Max(*limit-*used, 0)
	return &remaining
}

func parseWorkspaceQuota(raw json.RawMessage) (store.WarpQuotaWindow, bool) {
	window := store.WarpQuotaWindow{Scope: "workspace_monthly", Unit: "credits"}
	var workspace struct {
		Included json.RawMessage `json:"includedUsageInfo"`
	}
	if json.Unmarshal(raw, &workspace) != nil || !quotaPresent(workspace.Included) {
		return window, false
	}
	var included struct {
		Limit quotaNumber     `json:"includedUsageCredits"`
		Used  quotaNumber     `json:"usageCreditsUsedSinceLastRefresh"`
		Reset json.RawMessage `json:"nextRefreshTime"`
	}
	if json.Unmarshal(workspace.Included, &included) != nil {
		return window, false
	}
	window.Limit, window.Used = included.Limit.value, included.Used.value
	window.Remaining = quotaRemaining(window.Limit, window.Used)
	var resetValid bool
	window.ResetAt, resetValid = quotaReset(included.Reset)
	return window, window.Limit != nil && window.Used != nil && resetValid
}

// requestLimitInfo retains its historical name, but the current official UI
// treats this as the included personal credit pool. Never sum it with teams.
// An explicitly unlimited window needs no invented limit, usage, or reset.
func parseLegacyQuota(raw json.RawMessage) (store.WarpQuotaWindow, bool) {
	window := store.WarpQuotaWindow{Scope: "personal", Unit: "credits"}
	var legacy struct {
		Unlimited json.RawMessage `json:"isUnlimited"`
		Limit     quotaNumber     `json:"requestLimit"`
		Used      quotaNumber     `json:"requestsUsedSinceLastRefresh"`
		Reset     json.RawMessage `json:"nextRefreshTime"`
	}
	if json.Unmarshal(raw, &legacy) != nil {
		return window, false
	}
	window.HasUnlimited = quotaPresent(legacy.Unlimited) && json.Unmarshal(legacy.Unlimited, &window.Unlimited) == nil
	window.Limit, window.Used = legacy.Limit.value, legacy.Used.value
	reset, resetValid := quotaReset(legacy.Reset)
	window.ResetAt = reset
	if window.Unlimited {
		// Null/missing values are unknown, but malformed supplied measurements
		// must not turn an invalid legacy object into a successful observation.
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		if (quotaPresent(fields["requestLimit"]) && window.Limit == nil) ||
			(quotaPresent(fields["requestsUsedSinceLastRefresh"]) && window.Used == nil) ||
			(quotaPresent(legacy.Reset) && !resetValid) {
			return window, false
		}
		return window, true
	}
	window.Remaining = quotaRemaining(window.Limit, window.Used)
	return window, window.HasUnlimited && window.Limit != nil && window.Used != nil && resetValid
}

// FetchQuota observes current upstream windows without persisting quota state.
// Partial observations retain unknown fields as nil and never infer bonuses.
func (c *Client) FetchQuota(ctx context.Context) (store.WarpQuotaSnapshot, error) {
	return c.fetchQuota(ctx, nil)
}

// observedRefresh binds this response to the credential used for the request,
// not to whatever a concurrent operation leaves on Client.account afterwards.
func (c *Client) fetchQuota(ctx context.Context, observedRefresh *string) (store.WarpQuotaSnapshot, error) {
	snapshot := store.WarpQuotaSnapshot{AttemptedAt: time.Now().UTC(), Status: "error", Source: quotaSource}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	token, err := c.accessToken(ctx)
	if err != nil {
		return snapshot, err
	}
	if observedRefresh != nil {
		c.mu.Lock()
		grant := credentials(c.account)
		if grant.AccessToken != token {
			c.mu.Unlock()
			return snapshot, errCredential
		}
		*observedRefresh = grant.RefreshToken
		c.mu.Unlock()
	}
	payload, _ := json.Marshal(map[string]any{
		"query": quotaQuery, "operationName": "GetRequestLimitInfo",
		"variables": map[string]any{"requestContext": map[string]any{
			"clientContext": map[string]any{"version": clientVersion},
			"osContext":     map[string]any{"category": "Linux", "name": "Linux", "version": "", "linuxKernelVersion": nil},
		}},
	})
	var answer struct {
		Data struct {
			User struct {
				Type string `json:"__typename"`
				User struct {
					Workspaces json.RawMessage `json:"workspaces"`
					Legacy     json.RawMessage `json:"requestLimitInfo"`
				} `json:"user"`
			} `json:"user"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := c.json(ctx, c.base+"/graphql/v2?op=GetRequestLimitInfo", "application/json", payload, token, &answer); err != nil {
		return snapshot, err
	}
	if len(answer.Errors) != 0 || answer.Data.User.Type != "UserOutput" {
		return snapshot, errResponse
	}
	user := answer.Data.User.User
	var workspaces []json.RawMessage
	workspaceValid := quotaPresent(user.Workspaces) && json.Unmarshal(user.Workspaces, &workspaces) == nil
	partial := !workspaceValid
	measured := false
	for _, raw := range workspaces {
		window, full := parseWorkspaceQuota(raw)
		snapshot.Windows = append(snapshot.Windows, window)
		partial = partial || !full
		measured = measured || window.Limit != nil || window.Used != nil
	}
	if quotaPresent(user.Legacy) {
		window, valid := parseLegacyQuota(user.Legacy)
		snapshot.Windows = append(snapshot.Windows, window)
		measured = measured || window.Limit != nil || window.Used != nil || (window.HasUnlimited && window.Unlimited)
		if !valid {
			partial = true
		}
	}
	if !measured {
		return snapshot, errResponse
	}
	snapshot.Status = "ok"
	if partial {
		snapshot.Status = "partial"
	}
	// Order observations by request start, not completion: a slow older read
	// must not replace a later-started result. Both stamps describe that read.
	snapshot.SyncedAt = snapshot.AttemptedAt
	return snapshot, nil
}
