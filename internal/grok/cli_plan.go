package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"orchids-api/internal/store"
)

func cliPlanNameAvailable(tier string) bool {
	return strings.TrimSpace(tier) != "" && !strings.EqualFold(strings.TrimSpace(tier), "unknown")
}

// The CLI settings endpoint names Free even when /user has a null tier and
// the access token omits tier. Only an explicit label establishes a plan.
func (c *CLIClient) fetchSettingsSubscriptionTier(ctx context.Context, acc *store.Account) (string, error) {
	payload, err := c.fetchCLIPlanPayload(ctx, acc, "/settings")
	if err != nil {
		return "", err
	}
	return cliBillingString(payload, "subscription_tier_display", "subscriptionTierDisplay"), nil
}

func (c *CLIClient) fetchSubscriptionTier(ctx context.Context, acc *store.Account) (string, error) {
	payload, err := c.fetchCLIPlanPayload(ctx, acc, "/user?include=subscription")
	if err != nil {
		return "", err
	}
	if tier := cliBillingString(payload, "subscriptionTier", "subscription_tier"); cliPlanNameAvailable(tier) {
		return tier, nil
	}
	var user map[string]json.RawMessage
	_ = json.Unmarshal(payload["user"], &user)
	return cliBillingString(user, "subscriptionTier", "subscription_tier"), nil
}

func (c *CLIClient) fetchCLIPlanPayload(ctx context.Context, acc *store.Account, path string) (map[string]json.RawMessage, error) {
	resp, err := c.request(ctx, acc, http.MethodGet, c.baseURL()+path, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, cliOAuthMaxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, newCLIUpstreamError(resp.StatusCode, resp.Header, body)
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return nil, fmt.Errorf("decode grok cli plan response: invalid object")
	}
	return payload, nil
}
