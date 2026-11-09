package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// The SSO account management surface. Cookies are write-only: they are
// accepted here and never returned; every response names accounts by ID and a
// masked token.

const (
	maxSSOImportTokens   = 2000
	maxSSOImportBodySize = 4 << 20
	ssoRefreshWorkers    = 5
)

type ssoImportRequest struct {
	// Text is the pasted (or TXT/JSON file) content: one cookie per line, a
	// "sso=...; sso-rw=..." cookie string, a JSON array of strings, or a JSON
	// array of objects carrying a "token"/"sso" field.
	Text   string   `json:"text"`
	Tokens []string `json:"tokens"`
	// Pool is "auto" (classify from the first quota read), "basic", "super" or
	// "heavy".
	Pool       string `json:"pool"`
	NamePrefix string `json:"name_prefix"`
	Weight     int    `json:"weight"`
	EnableNSFW bool   `json:"enable_nsfw"`
	Refresh    *bool  `json:"refresh"`
}

type ssoImportItem struct {
	Token     string `json:"token"`
	Status    string `json:"status"`
	AccountID int64  `json:"account_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

var ssoSeparator = regexp.MustCompile(`[\r\n,]+`)

// parseSSOImportTokens extracts SSO cookie values from any of the accepted
// import shapes, normalizes them and removes duplicates in input order.
func parseSSOImportTokens(text string, tokens []string) []string {
	raw := append([]string(nil), tokens...)
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		raw = append(raw, jsonSSOTokens(trimmed)...)
	} else if trimmed != "" {
		for line := range strings.SplitSeq(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.Contains(line, ";") || strings.Contains(strings.ToLower(line), "sso=") {
				raw = append(raw, line)
				continue
			}
			raw = append(raw, ssoSeparator.Split(line, -1)...)
		}
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		token := NormalizeSSOToken(strings.Trim(strings.TrimSpace(value), `"'`))
		if token == "" || strings.ContainsAny(token, " \t") {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

func jsonSSOTokens(text string) []string {
	var decoded any
	if json.Unmarshal([]byte(text), &decoded) != nil {
		return nil
	}
	var out []string
	var walk func(value any)
	walk = func(value any) {
		switch v := value.(type) {
		case string:
			out = append(out, v)
		case []any:
			for _, item := range v {
				walk(item)
			}
		case map[string]any:
			for _, key := range []string{"token", "sso", "cookie", "client_cookie"} {
				if s, ok := v[key].(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, s)
					return
				}
			}
			// grok2api exports pools as {"ssoBasic": [...], "ssoSuper": [...]}.
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(decoded)
	return out
}

func subscriptionForPool(pool string) string {
	switch normalizeGrokPoolName(pool) {
	case "heavy":
		return "heavy"
	case "super":
		return "super"
	case "basic":
		if strings.EqualFold(strings.TrimSpace(pool), "auto") || strings.TrimSpace(pool) == "" {
			return ""
		}
		return "basic"
	}
	return ""
}

// HandleSSOImport creates Web SSO accounts from pasted or uploaded cookies.
// POST /api/grok/sso/import
func (h *Handler) HandleSSOImport(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		httpserver.WriteError(w, http.StatusServiceUnavailable, "store not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSSOImportBodySize)
	var req ssoImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid json")
		return
	}
	tokens := parseSSOImportTokens(req.Text, req.Tokens)
	if len(tokens) == 0 {
		httpserver.WriteError(w, http.StatusBadRequest, "no sso token found")
		return
	}
	if len(tokens) > maxSSOImportTokens {
		httpserver.WriteError(w, http.StatusBadRequest, fmt.Sprintf("at most %d tokens per import", maxSSOImportTokens))
		return
	}
	ctx := r.Context()
	st := h.lb.Store
	subscription := subscriptionForPool(req.Pool)
	weight := req.Weight
	if weight <= 0 {
		weight = 1
	}
	prefix := strings.TrimSpace(req.NamePrefix)
	items := make([]ssoImportItem, 0, len(tokens))
	created := make([]*store.Account, 0, len(tokens))
	counts := map[string]int{}
	for _, token := range tokens {
		item := ssoImportItem{Token: maskToken(token)}
		acc := &store.Account{
			AccountType:  "grok",
			ClientCookie: "sso=" + token,
			Enabled:      true,
			Weight:       weight,
			Subscription: subscription,
		}
		if prefix != "" {
			acc.Name = prefix + "-" + util.Fingerprint(token)[:8]
		}
		if err := PrepareAccount(acc); err != nil {
			item.Status, item.Error = "invalid", err.Error()
			counts[item.Status]++
			items = append(items, item)
			continue
		}
		duplicate, err := st.CreateAccountUnlessDuplicate(ctx, acc, func(other *store.Account) bool {
			return IsWebAccount(other) && SSOToken(other) == token
		})
		switch {
		case err != nil:
			item.Status, item.Error = "failed", "failed to create account"
			slog.Warn("grok sso import: create failed", "token", maskToken(token), "error", err)
		case duplicate != nil:
			item.Status, item.AccountID = "duplicate", duplicate.ID
		default:
			item.Status, item.AccountID = "created", acc.ID
			created = append(created, acc)
		}
		counts[item.Status]++
		items = append(items, item)
	}

	refresh := req.Refresh == nil || *req.Refresh
	if refresh || req.EnableNSFW {
		// The probe and the NSFW switch talk to grok.com once per account; they
		// run after the response so a large import is not held open by them.
		accounts := append([]*store.Account(nil), created...)
		enableNSFW := req.EnableNSFW
		go h.postImportMaintenance(context.WithoutCancel(ctx), accounts, refresh, enableNSFW)
	}
	util.WriteJSON(w, map[string]any{
		"status":     "success",
		"total":      len(tokens),
		"created":    counts["created"],
		"duplicate":  counts["duplicate"],
		"invalid":    counts["invalid"],
		"failed":     counts["failed"],
		"items":      items,
		"refreshing": refresh && len(created) > 0,
	})
}

func (h *Handler) postImportMaintenance(ctx context.Context, accounts []*store.Account, refresh, enableNSFW bool) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if refresh {
		h.refreshAccounts(ctx, accounts)
	}
	if enableNSFW {
		for _, acc := range accounts {
			if ctx.Err() != nil {
				return
			}
			if _, err := h.enableNSFWForAccount(ctx, acc); err != nil {
				slog.Warn("grok sso import: enable nsfw failed", "account_id", acc.ID, "error", err)
			}
		}
	}
}

type ssoAccountsRequest struct {
	AccountIDs []int64 `json:"account_ids"`
}

type ssoAccountResult struct {
	AccountID int64  `json:"account_id"`
	Name      string `json:"name"`
	Success   bool   `json:"success"`
	Status    string `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
}

// resolveSSOAccounts returns the Web accounts the request names, or every Web
// account when it names none.
func (h *Handler) resolveSSOAccounts(r *http.Request) ([]*store.Account, error) {
	var req ssoAccountsRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid json")
	}
	accounts, err := h.lb.Store.ListAccounts(r.Context())
	if err != nil {
		return nil, fmt.Errorf("failed to list accounts")
	}
	wanted := map[int64]bool{}
	for _, id := range req.AccountIDs {
		wanted[id] = true
	}
	out := make([]*store.Account, 0, len(accounts))
	for _, acc := range accounts {
		if !IsWebAccount(acc) || (len(wanted) > 0 && !wanted[acc.ID]) {
			continue
		}
		out = append(out, acc)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no grok sso accounts selected")
	}
	return out, nil
}

// refreshAccounts verifies each account against grok.com, persists the
// observation and republishes the Web routes once at the end.
func (h *Handler) refreshAccounts(ctx context.Context, accounts []*store.Account) []ssoAccountResult {
	results := make([]ssoAccountResult, len(accounts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, ssoRefreshWorkers)
	cfg := h.configSnapshot()
	for i, acc := range accounts {
		wg.Add(1)
		go func(i int, acc *store.Account) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = ssoAccountResult{AccountID: acc.ID, Name: acc.Name, Error: ctx.Err().Error()}
				return
			}
			defer func() { <-sem }()
			updated := *acc
			status, err := VerifyAccount(ctx, cfg, &updated)
			now := time.Now()
			result := ssoAccountResult{AccountID: acc.ID, Name: acc.Name, Success: err == nil, Status: status}
			switch {
			case status == "401":
				updated.StatusCode = "401"
				updated.AuthStatus = store.AccountAuthStatusReauthRequired
				updated.StatusMessage = "grok.com 已不接受该 SSO Cookie，需要重新导入"
				updated.LastAttempt = now
				result.Error = "sso cookie rejected"
			case err != nil:
				result.Error = redactSSOError(err, acc)
			default:
				if code := strings.TrimSpace(updated.StatusCode); code == "401" || code == "403" {
					updated.StatusCode = ""
					updated.StatusMessage = ""
				}
				updated.VerifiedAt = now
			}
			if err := h.lb.Store.UpdateAccount(ctx, &updated); err != nil {
				slog.Warn("grok sso refresh: update failed", "account_id", acc.ID, "error", err)
			}
			results[i] = result
		}(i, acc)
	}
	wg.Wait()
	if err := PublishRoutes(ctx, h.lb.Store); err != nil {
		slog.Warn("grok web route publication failed", "error", err)
	}
	return results
}

func redactSSOError(err error, acc *store.Account) string {
	if err == nil {
		return ""
	}
	if isEgressChallengeError(err) || strings.Contains(err.Error(), "Just a moment") {
		return "grok.com 返回了 Cloudflare 验证：请在配置中设置 grok_cf_clearance，或配置 FlareSolverr / 代理出口"
	}
	return grokbase.DiagnosticText(err.Error(), acc)
}

// HandleSSORefresh re-reads identity and quota for the named Web accounts.
// POST /api/grok/sso/refresh {"account_ids":[...]}
func (h *Handler) HandleSSORefresh(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		httpserver.WriteError(w, http.StatusServiceUnavailable, "store not configured")
		return
	}
	accounts, err := h.resolveSSOAccounts(r)
	if err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	results := h.refreshAccounts(ctx, accounts)
	ok := 0
	for _, result := range results {
		if result.Success {
			ok++
		}
	}
	util.WriteJSON(w, map[string]any{"status": "success", "total": len(results), "ok": ok, "failed": len(results) - ok, "results": results})
}

// enableNSFWForAccount switches the grok.com NSFW preference on for one
// account (accepting the terms of service and setting a birth date first, as
// grok.com requires) and records it.
func (h *Handler) enableNSFWForAccount(ctx context.Context, acc *store.Account) (NSFWEnableResult, error) {
	token := SSOToken(acc)
	if token == "" {
		return NSFWEnableResult{}, ErrMissingSSOToken
	}
	callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	result := h.webClient().EnableNSFWDetailed(callCtx, token)
	if !result.Success {
		return result, fmt.Errorf("enable nsfw failed: %s", firstNonEmpty(result.Error, result.GRPCMessage, fmt.Sprint(result.HTTPStatus)))
	}
	updated := *acc
	updated.NSFWEnabled = true
	if err := h.lb.Store.UpdateAccount(callCtx, &updated); err != nil {
		return result, err
	}
	return result, nil
}

// HandleSSONSFW enables NSFW on the named Web accounts.
// POST /api/grok/sso/nsfw {"account_ids":[...]}
func (h *Handler) HandleSSONSFW(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		httpserver.WriteError(w, http.StatusServiceUnavailable, "store not configured")
		return
	}
	accounts, err := h.resolveSSOAccounts(r)
	if err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	results := make([]ssoAccountResult, len(accounts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, ssoRefreshWorkers)
	for i, acc := range accounts {
		wg.Add(1)
		go func(i int, acc *store.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_, err := h.enableNSFWForAccount(ctx, acc)
			results[i] = ssoAccountResult{AccountID: acc.ID, Name: acc.Name, Success: err == nil}
			if err != nil {
				results[i].Error = redactSSOError(err, acc)
			}
		}(i, acc)
	}
	wg.Wait()
	ok := 0
	for _, result := range results {
		if result.Success {
			ok++
		}
	}
	util.WriteJSON(w, map[string]any{"status": "success", "total": len(results), "ok": ok, "failed": len(results) - ok, "results": results})
}

// HandleAvailability reports how many usable accounts each Grok plane has, so
// the tools page can tell which of its features can run.
// GET /api/grok/availability
func (h *Handler) HandleAvailability(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodGet) {
		return
	}
	counts := map[string]int{ProviderBuild: 0, ProviderWeb: 0}
	webSynced, webError := 0, ""
	// plans counts the active Web accounts by plan, so the tools page can tell
	// a Free-only pool. media reports whether any of them still has the
	// allowance of each media product (the same fence account selection uses).
	plans := map[string]int{"free": 0, "paid": 0, "unknown": 0}
	media := map[string]bool{mediaImagePro: false, mediaImageEdit: false}
	now := time.Now()
	if h != nil && h.lb != nil && h.lb.Store != nil {
		accounts, err := h.lb.Store.ListAccounts(r.Context())
		if err != nil {
			httpserver.WriteError(w, http.StatusInternalServerError, "failed to list accounts")
			return
		}
		for _, acc := range accounts {
			if acc == nil || !acc.Enabled || !store.AccountAuthActive(acc) {
				continue
			}
			if provider := ProviderForAccount(acc); provider != "" {
				counts[provider]++
			}
			if IsWebAccount(acc) {
				plans[webPlanTier(acc)]++
				for product := range media {
					media[product] = media[product] || mediaAllowed(acc, product, now)
				}
				if accountObservesRoutes(acc, now) {
					webSynced++
				} else if webError == "" {
					webError = acc.GrokWebSyncError
				}
			}
		}
	}
	util.WriteJSON(w, map[string]any{"counts": counts, "web_synced": webSynced, "web_sync_error": webError, "web_plans": plans, "media": media})
}
