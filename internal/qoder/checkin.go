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
	"slices"
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
	// The campaign view is the desktop client's activity iframe, served from
	// the OpenAPI host itself. checkinClientType, checkinClientVersion and
	// checkinUserAgent match a capture of the international desktop client
	// 0.4.8 claiming the daily credits. The service filters the campaign list
	// by client; the 0.3.4 version the reference sent saw only a promotion.
	checkinClientType    = "10"
	checkinClientVersion = "0.4.8"
	checkinUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Qoder/" + checkinClientVersion + " Chrome/150.0.7871.114 Electron/43.1.1 Safari/537.36"
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
	raw, err := c.campaignRequest(ctx, creds, http.MethodGet, checkinCampaignsPath)
	if err != nil {
		var httpErr *checkinHTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound {
			return provider.CheckinResult{Status: provider.CheckinSkipped, Message: "暂无可签到活动（活动接口返回 404）"}, nil
		}
		return provider.CheckinResult{}, err
	}
	listed := campaignsFrom(raw)
	credits := creditCampaigns(listed)
	if len(credits) == 0 {
		// Say what the service returned: an unexpected shape and a list without
		// a credit campaign need different fixes, and both used to read as the
		// same bare "nothing to claim".
		return provider.CheckinResult{Status: provider.CheckinSkipped, Message: "暂无可签到活动（" + describeCampaigns(raw, listed) + "）"}, nil
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
		return provider.CheckinResult{Status: provider.CheckinSkipped, Message: "暂无可签到活动（" + describeCampaigns(raw, listed) + "）"}, nil
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
	// Header order and values follow the desktop capture; the browser-only
	// fetch metadata is included because the iframe always sends it.
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	req.Header.Set("User-Agent", checkinUserAgent)
	req.Header.Set("Sec-Ch-Ua", `"Not;A=Brand";v="8", "Chromium";v="150"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", base)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Referer", base+"/growth-page/activity-iframe")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(creds.AccessToken))
	req.Header.Set("Cosy-ClientType", checkinClientType)
	req.Header.Set("Cosy-Version", checkinClientVersion)
	req.Header.Set("Cosy-MachineOS", machineOS)
	machineID := c.machineID
	if device, ok := parseCheckinDevice(c.checkinDevice); ok {
		// The configured desktop fingerprint replaces the derived one: the
		// service withholds the daily credit campaign from any other identity.
		machineID = util.FirstNonEmpty(device.MachineID, machineID)
		if device.MachineHostname != "" {
			req.Header.Set("Cosy-MachineHostname", device.MachineHostname)
		}
		req.Header.Set("Cosy-MachineId", machineID)
		req.Header.Set("Cosy-MachineToken", device.MachineToken)
		req.Header.Set("Cosy-MachineCode", device.MachineCode)
		req.Header.Set("Cosy-MachineType", device.MachineType)
	} else {
		req.Header.Set("Cosy-MachineId", machineID)
		req.Header.Set("Cosy-MachineToken", c.machineTokenOr(c.machineID))
		// The service drops the credit campaign for a partial identity, so the
		// fourth identity header must always be present.
		req.Header.Set("Cosy-MachineCode", checkinMachineCode(c.machineID))
		req.Header.Set("Cosy-MachineType", c.machineTypeOr(machineSceneType))
	}

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

// checkinDevice is the desktop fingerprint the umid runtime-info helper
// prints. Its vmInfo and any other fields are ignored.
type checkinDevice struct {
	MachineToken string `json:"machineToken"`
	MachineType  string `json:"machineType"`
	MachineCode  string `json:"machineCode"`
	// MachineID and MachineHostname are optional: the helper does not print
	// them, but the desktop client sends both, so an operator may add them
	// from a capture of the same machine.
	MachineID       string `json:"machineId,omitempty"`
	MachineHostname string `json:"machineHostname,omitempty"`
}

// parseCheckinDevice reads the helper output as pasted: the first JSON object
// in the text. A fingerprint missing any of the three fields is unusable,
// because a partial identity is treated as an unofficial client.
func parseCheckinDevice(raw string) (checkinDevice, bool) {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return checkinDevice{}, false
	}
	var device checkinDevice
	if json.NewDecoder(strings.NewReader(raw[start:])).Decode(&device) != nil {
		return checkinDevice{}, false
	}
	device.MachineToken = strings.TrimSpace(device.MachineToken)
	device.MachineType = strings.TrimSpace(device.MachineType)
	device.MachineCode = strings.TrimSpace(device.MachineCode)
	device.MachineID = strings.TrimSpace(device.MachineID)
	device.MachineHostname = strings.TrimSpace(device.MachineHostname)
	return device, device.MachineToken != "" && device.MachineType != "" && device.MachineCode != ""
}

// checkinMachineCode derives the stable Cosy-MachineCode for the login device.
// The desktop client computes it from hardware; one value per device identity
// keeps it constant across calls and restarts.
func checkinMachineCode(machineID string) string {
	sum := md5.Sum([]byte("machinecode:" + strings.TrimSpace(machineID)))
	return hex.EncodeToString(sum[:])
}

// campaignsFrom finds the campaign list wherever the response nests it: the
// first array of objects carrying a campaignId, searched breadth-first a few
// levels deep. The reference read only campaigns, data.campaigns and data[];
// any other envelope parsed as an empty list and reported "nothing to claim".
func campaignsFrom(raw []byte) []campaign {
	var root any
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	level := []any{root}
	for depth := 0; depth < 5 && len(level) > 0; depth++ {
		var next []any
		for _, node := range level {
			switch typed := node.(type) {
			case []any:
				if isCampaignList(typed) {
					encoded, _ := json.Marshal(typed)
					var list []campaign
					if json.Unmarshal(encoded, &list) == nil {
						return list
					}
				}
				next = append(next, typed...)
			case map[string]any:
				for _, child := range typed {
					next = append(next, child)
				}
			}
		}
		level = next
	}
	return nil
}

func isCampaignList(items []any) bool {
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			if _, ok := object["campaignId"]; ok {
				return true
			}
		}
	}
	return false
}

// describeCampaigns summarizes a list response for the outcome message:
// each campaign's action, benefit kind and claim status, or the top-level
// keys when no list was found. It carries no identifiers or amounts.
func describeCampaigns(raw []byte, listed []campaign) string {
	if len(listed) == 0 {
		var top map[string]json.RawMessage
		if json.Unmarshal(raw, &top) != nil {
			return "响应不是 JSON 对象"
		}
		keys := make([]string, 0, len(top))
		for key := range top {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		return "未找到活动列表，响应字段: " + strings.Join(keys, ",")
	}
	parts := make([]string, 0, len(listed))
	for _, item := range listed {
		parts = append(parts, util.FirstNonEmpty(item.ActionType, "?")+"/"+util.FirstNonEmpty(item.Benefit.Kind, "?")+"/"+util.FirstNonEmpty(item.ClaimStatus, "?"))
	}
	return fmt.Sprintf("共 %d 个活动，无可领积分: %s", len(listed), util.Truncate(strings.Join(parts, "; "), 200))
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
