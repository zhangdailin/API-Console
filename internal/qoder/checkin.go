package qoder

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/provider"
	"orchids-api/internal/util"
)

// Daily credit check-in against the OpenAPI campaigns service, as the desktop
// client and the reference bridge (EchoPing07/Qoder-2API-Go PR #14) do it:
//
//	GET  {openapi}/sash/api/v1/me/campaigns            list today's campaigns
//	POST {openapi}/sash/api/v1/me/campaigns/{id}/claim claim one campaign
//
// A daily credit campaign is a CLAIM_BENEFIT entry whose benefit kind is
// CREDITS; it is claimable while claimStatus is CLAIMABLE and done once it is
// CLAIMED. The international service rejects the COSY token here and accepts
// the OpenAPI bearer, which for this channel is the device access token the
// quota read already uses.
//
// The reference notes that the service exposes the daily credit campaign only
// to the desktop client's genuine device fingerprint. This channel presents its
// own login device identity, so a "skipped" outcome may mean the campaign was
// withheld rather than absent; whether the QoderWork identity qualifies is
// only established by a live run.

const (
	checkinCampaignsPath = "/sash/api/v1/me/campaigns"
	// checkinOrigin is the page the desktop client loads the campaign iframe
	// from for the international service.
	checkinOrigin = "https://qoder.sh"
	// checkinClientType and checkinClientVersion identify the desktop client
	// whose campaign view the service serves.
	checkinClientType    = "10"
	checkinClientVersion = "0.3.4"
	// checkinBodyLimit bounds a campaigns response; it is a short JSON list.
	checkinBodyLimit = 64 << 10
)

// checkinHTTPError carries the campaigns service status so a 404 (nothing open
// today) and a 401 (token refused) take their own paths. Body is truncated.
type checkinHTTPError struct {
	Status int
	Body   string
}

func (e *checkinHTTPError) Error() string {
	return fmt.Sprintf("qoder check-in: HTTP %d: %s", e.Status, e.Body)
}

type campaign struct {
	CampaignID  string `json:"campaignId"`
	ActionType  string `json:"actionType"`
	ClaimStatus string `json:"claimStatus"`
	Benefit     struct {
		Kind   string  `json:"kind"`
		Amount float64 `json:"amount"`
	} `json:"benefit"`
}

// Checkin claims today's credit campaigns for the account. A refused token is
// refreshed once and the call repeated.
func (c *Client) Checkin(ctx context.Context) (provider.CheckinResult, error) {
	if c == nil {
		return provider.CheckinResult{}, fmt.Errorf("qoder client is nil")
	}
	creds, err := c.ensureAccessToken(ctx)
	if err != nil {
		return provider.CheckinResult{}, err
	}
	result, err := c.checkinWith(ctx, creds)
	var httpErr *checkinHTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnauthorized {
		return result, err
	}
	if refreshErr := c.forceRefresh(ctx, creds); refreshErr != nil {
		return provider.CheckinResult{}, fmt.Errorf("qoder check-in: refresh after 401 failed: %w", refreshErr)
	}
	return c.checkinWith(ctx, c.currentCredentials())
}

func (c *Client) checkinWith(ctx context.Context, creds Credentials) (provider.CheckinResult, error) {
	listed, err := c.listCampaigns(ctx, creds)
	if err != nil {
		var httpErr *checkinHTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound {
			return provider.CheckinResult{Status: provider.CheckinSkipped, Message: "暂无可签到活动"}, nil
		}
		return provider.CheckinResult{}, err
	}
	credits := creditCampaigns(listed)
	if len(credits) == 0 {
		return provider.CheckinResult{Status: provider.CheckinSkipped, Message: "暂无可签到活动"}, nil
	}
	var claimable []campaign
	alreadyClaimed := false
	for _, item := range credits {
		switch item.ClaimStatus {
		case "CLAIMABLE":
			claimable = append(claimable, item)
		case "CLAIMED":
			alreadyClaimed = true
		}
	}
	if len(claimable) == 0 {
		if alreadyClaimed {
			return provider.CheckinResult{Status: provider.CheckinAlready, Message: "今日已签到"}, nil
		}
		return provider.CheckinResult{Status: provider.CheckinSkipped, Message: "暂无可签到活动"}, nil
	}

	confirmed, recovered, reward := 0, 0, 0.0
	for _, item := range claimable {
		status, claimErr := c.claimCampaign(ctx, creds, item.CampaignID)
		if claimErr != nil || status != "CLAIMED" {
			// The service sometimes records a claim it did not confirm, and an
			// error can still follow a recorded claim. The list is the authority.
			relisted, relistErr := c.listCampaigns(ctx, creds)
			if relistErr != nil || claimStatusOf(relisted, item.CampaignID) != "CLAIMED" {
				if claimErr != nil {
					return provider.CheckinResult{}, claimErr
				}
				return provider.CheckinResult{}, fmt.Errorf("qoder check-in: claim of campaign %s was not confirmed", item.CampaignID)
			}
			recovered++
		} else {
			confirmed++
		}
		if item.Benefit.Amount > 0 {
			reward += item.Benefit.Amount
		}
	}
	if confirmed == 0 && recovered == 0 {
		return provider.CheckinResult{}, fmt.Errorf("qoder check-in: no claim was confirmed")
	}
	result := provider.CheckinResult{Status: provider.CheckinSuccess, Message: "签到成功", RewardCredits: reward}
	if reward > 0 {
		result.Message = fmt.Sprintf("签到成功 +%g 积分", reward)
	}
	return result, nil
}

func (c *Client) listCampaigns(ctx context.Context, creds Credentials) ([]campaign, error) {
	raw, err := c.campaignRequest(ctx, creds, http.MethodGet, checkinCampaignsPath)
	if err != nil {
		return nil, err
	}
	return campaignsFrom(raw), nil
}

func (c *Client) claimCampaign(ctx context.Context, creds Credentials, id string) (string, error) {
	raw, err := c.campaignRequest(ctx, creds, http.MethodPost, checkinCampaignsPath+"/"+url.PathEscape(id)+"/claim")
	if err != nil {
		return "", err
	}
	var claimed struct {
		Status string `json:"status"`
		Data   *struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &claimed) != nil {
		return "", nil
	}
	if claimed.Data != nil && claimed.Data.Status != "" {
		return claimed.Data.Status, nil
	}
	return claimed.Status, nil
}

func (c *Client) campaignRequest(ctx context.Context, creds Credentials, method, path string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	base := strings.TrimRight(c.endpoints.openAPI, "/")
	req, err := http.NewRequestWithContext(reqCtx, method, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(creds.AccessToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Qoder")
	req.Header.Set("Cosy-ClientType", checkinClientType)
	req.Header.Set("Cosy-Version", checkinClientVersion)
	req.Header.Set("Cosy-MachineId", c.machineID)
	req.Header.Set("Cosy-MachineToken", c.machineTokenOr(c.machineID))
	req.Header.Set("Cosy-MachineType", c.machineTypeOr(machineSceneType))
	// The service drops the credit campaign for a partial identity, so the
	// fourth identity header must always be present.
	req.Header.Set("Cosy-MachineCode", checkinMachineCode(c.machineID))
	req.Header.Set("Cosy-MachineOS", machineOS)
	req.Header.Set("Origin", checkinOrigin)
	req.Header.Set("Referer", base+"/growth-page/activity-iframe")

	// A refused token is answered with a redirect to a login page; following it
	// would read that page as the campaign list.
	client := &http.Client{
		Transport: c.control.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, checkinBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("qoder check-in: read response: %w", err)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		status := resp.StatusCode
		if status >= 300 && status < 400 {
			// A login redirect is the service refusing the token.
			status = http.StatusUnauthorized
		}
		return nil, &checkinHTTPError{Status: status, Body: util.Truncate(strings.Join(strings.Fields(string(body)), " "), 200)}
	}
	return body, nil
}

// checkinMachineCode derives the stable Cosy-MachineCode for the login device.
// The desktop client computes it from hardware; one value per device identity
// keeps it constant across calls and restarts.
func checkinMachineCode(machineID string) string {
	sum := md5.Sum([]byte("machinecode:" + strings.TrimSpace(machineID)))
	return hex.EncodeToString(sum[:])
}

// campaignsFrom accepts the list at the top level or under data.
func campaignsFrom(raw []byte) []campaign {
	var shaped struct {
		Campaigns []campaign      `json:"campaigns"`
		Data      json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &shaped) != nil {
		return nil
	}
	if len(shaped.Campaigns) > 0 {
		return shaped.Campaigns
	}
	var nested struct {
		Campaigns []campaign `json:"campaigns"`
	}
	if json.Unmarshal(shaped.Data, &nested) == nil && len(nested.Campaigns) > 0 {
		return nested.Campaigns
	}
	var list []campaign
	if json.Unmarshal(shaped.Data, &list) == nil {
		return list
	}
	return nil
}

// creditCampaigns keeps the daily credit campaigns. Other CLAIM_BENEFIT
// activities grant non-credit perks and must not read as "already checked in".
func creditCampaigns(items []campaign) []campaign {
	out := make([]campaign, 0, len(items))
	for _, item := range items {
		if item.ActionType == "CLAIM_BENEFIT" && strings.TrimSpace(item.CampaignID) != "" && item.Benefit.Kind == "CREDITS" {
			out = append(out, item)
		}
	}
	return out
}

func claimStatusOf(items []campaign, id string) string {
	for _, item := range items {
		if item.CampaignID == id {
			return item.ClaimStatus
		}
	}
	return ""
}

// checkinRetryAfterFailure is how long a failed scheduled check-in waits
// before the scheduler tries the same day again.
const checkinRetryAfterFailure = time.Hour

// checkinDue reports whether the scheduler should check the account in now:
// it opted in, and today (server local time) has no settled outcome yet. A
// failure is retried after checkinRetryAfterFailure.
func checkinDue(enabled bool, last time.Time, lastStatus string, now time.Time) bool {
	if !enabled {
		return false
	}
	if last.IsZero() {
		return true
	}
	if lastStatus == provider.CheckinFailed {
		return now.Sub(last) >= checkinRetryAfterFailure
	}
	ly, lm, ld := last.In(now.Location()).Date()
	ny, nm, nd := now.Date()
	return ly != ny || lm != nm || ld != nd
}
