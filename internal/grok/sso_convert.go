package grok

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/grok/console"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/grok/web"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// Web → Build conversion: a Web SSO cookie is a signed-in xAI session, so it
// can approve the Build CLI's own device authorization. The device and token
// requests are the official flow (DeviceAuthenticator); only the approval is
// done with the cookie (web.ApproveDeviceCode). The result is an ordinary
// Build OAuth account; the Web account is left untouched.

const (
	ssoConvertWorkers     = 2
	ssoConvertPollCeiling = 75 * time.Second
	ssoConvertTimeout     = 2 * time.Minute
)

// ssoConvertResult reports one conversion. It never carries a credential.
type ssoConvertResult struct {
	AccountID int64  `json:"account_id"`
	Name      string `json:"name"`
	Success   bool   `json:"success"`
	// BuildAccountID is the converted account: the Build account, or the
	// Console account for a Console conversion.
	BuildAccountID int64  `json:"build_account_id,omitempty"`
	Created        bool   `json:"created,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Error          string `json:"error,omitempty"`
}

// convertWebGrant runs the device authorization for one Web account and
// returns the Build grant it produced.
func convertWebGrant(ctx context.Context, cfg *config.Config, acc *store.Account) (deviceGrant, error) {
	authenticator := NewDeviceAuthenticator(cfg)
	details, err := authenticator.Start(ctx)
	if err != nil {
		return deviceGrant{}, err
	}
	if err := web.ApproveDeviceCode(ctx, cfg, web.SSOToken(acc), details.UserCode, details.DeviceCode); err != nil {
		return deviceGrant{}, err
	}
	interval := max(time.Duration(details.Interval)*time.Second, time.Second)
	deadline := time.Now().Add(min(time.Duration(details.ExpiresIn)*time.Second, ssoConvertPollCeiling))
	for {
		accessToken, refreshToken, identityToken, expiresAt, err := authenticator.Exchange(ctx, details.DeviceCode)
		if err == nil {
			return deviceGrant{accessToken: accessToken, refreshToken: refreshToken, identityToken: identityToken, expiresAt: expiresAt}, nil
		}
		slowDown, pending := IsDeviceAuthorizationPending(err)
		if !pending {
			return deviceGrant{}, err
		}
		if slowDown {
			interval += 5 * time.Second
		}
		if time.Now().Add(interval).After(deadline) {
			return deviceGrant{}, errors.New("the approved device authorization did not issue a token in time")
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return deviceGrant{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// storeConvertedBuildAccount writes the grant as a Build account: onto the
// row the same xAI user already has, or as a new one.
func storeConvertedBuildAccount(ctx context.Context, st *store.Store, source *store.Account, grant deviceGrant) (*store.Account, bool, error) {
	fresh := accountFromDeviceGrant(grant)
	if fresh.Email != "" {
		fresh.Name = fresh.Email
	} else if name := strings.TrimSpace(source.Name); name != "" {
		fresh.Name = name + " Build"
	}
	identity := adminPort().IdentityKey(fresh)
	if identity != "" {
		accounts, err := st.ListAccounts(ctx)
		if err != nil {
			return nil, false, err
		}
		for _, existing := range accounts {
			if web.IsWebAccount(existing) || !accountIsOAuth(existing) || adminPort().IdentityKey(existing) != identity {
				continue
			}
			mergeDeviceLogin(existing, fresh)
			existing.AuthStatus = store.AccountAuthStatusActive
			if err := st.UpdateAccount(ctx, existing); err != nil {
				return nil, false, err
			}
			return existing, false, nil
		}
	}
	if err := st.CreateAccount(ctx, fresh); err != nil {
		return nil, false, err
	}
	return fresh, true, nil
}

// ssoConvertFailure maps a conversion error to a stable reason and an
// operator-facing message.
func ssoConvertFailure(err error, acc *store.Account) (reason, message string) {
	switch {
	case errors.Is(err, web.ErrBrowserVerificationRequired):
		return "browser_verification_required", "xAI 要求浏览器校验后才能批准授权，请改用设备授权在浏览器中手动完成"
	case errors.Is(err, web.ErrSSOSessionRejected), errors.Is(err, web.ErrMissingSSOToken):
		return "sso_rejected", "xAI 不接受该 SSO 会话，需要重新导入 Cookie"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout", "转换超时"
	default:
		return "failed", grokbase.DiagnosticText(err.Error(), acc)
	}
}

func (h *Handler) convertWebAccount(ctx context.Context, acc *store.Account) ssoConvertResult {
	result := ssoConvertResult{AccountID: acc.ID, Name: acc.Name}
	ctx, cancel := context.WithTimeout(ctx, ssoConvertTimeout)
	defer cancel()
	grant, err := convertWebGrant(ctx, h.configSnapshot(), acc)
	if err == nil {
		var build *store.Account
		build, result.Created, err = storeConvertedBuildAccount(ctx, h.lb.Store, acc, grant)
		if err == nil {
			result.Success, result.BuildAccountID = true, build.ID
			slog.Info("Converted a Grok Web account to Build", "web_account_id", acc.ID, "build_account_id", build.ID, "created", result.Created)
			return result
		}
		err = fmt.Errorf("store the Build account: %w", err)
	}
	result.Reason, result.Error = ssoConvertFailure(err, acc)
	slog.Warn("Grok Web to Build conversion failed", "web_account_id", acc.ID, "reason", result.Reason, "error", result.Error)
	return result
}

// HandleSSOConvertBuild converts the named Web accounts into Build accounts.
// POST /api/grok/sso/convert-build {"account_ids":[...]}
func (h *Handler) HandleSSOConvertBuild(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil || h.web == nil {
		httpserver.WriteError(w, http.StatusServiceUnavailable, "store not configured")
		return
	}
	accounts, err := h.web.ResolveSSOAccounts(r)
	if err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
	defer cancel()
	results := make([]ssoConvertResult, len(accounts))
	sem := make(chan struct{}, ssoConvertWorkers)
	var wg sync.WaitGroup
	for i, acc := range accounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = h.convertWebAccount(ctx, acc)
		}()
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

// HandleSSOConvertConsole creates a Console account for each named Web
// account. A Console account uses the same SSO cookie on console.x.ai, so the
// conversion copies the cookie, verifies it against Console and publishes the
// Console routes; the Web account is not changed. A Web account that already
// has a Console twin (same cookie) is reported, not duplicated.
// POST /api/grok/sso/convert-console {"account_ids":[...]}
func (h *Handler) HandleSSOConvertConsole(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil || h.web == nil {
		httpserver.WriteError(w, http.StatusServiceUnavailable, "store not configured")
		return
	}
	sources, err := h.web.ResolveSSOAccounts(r)
	if err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	st := h.lb.Store
	accounts, err := st.ListAccounts(ctx)
	if err != nil {
		httpserver.WriteError(w, http.StatusInternalServerError, "failed to list accounts")
		return
	}
	existing := map[string]*store.Account{}
	for _, acc := range accounts {
		if console.IsConsoleAccount(acc) {
			existing[console.CredentialKey(acc)] = acc
		}
	}
	results := make([]ssoConvertResult, 0, len(sources))
	cfg := h.configSnapshot()
	for _, source := range sources {
		result := ssoConvertResult{AccountID: source.ID, Name: source.Name}
		fresh := &store.Account{
			Name: util.FirstNonEmpty(strings.TrimSpace(source.Name), "Grok Console") + " Console", AccountType: "grok",
			GrokProvider: console.ProviderConsole, ClientCookie: web.SSOToken(source),
			Email: source.Email, UserID: source.UserID, Weight: max(source.Weight, 1), Enabled: true,
		}
		if err := console.PrepareAccount(fresh); err != nil {
			result.Reason, result.Error = "sso_rejected", "该 Web 账号没有 SSO Cookie"
			results = append(results, result)
			continue
		}
		if twin := existing[console.CredentialKey(fresh)]; twin != nil {
			result.Success, result.BuildAccountID, result.Reason = true, twin.ID, "already_exists"
			results = append(results, result)
			continue
		}
		status, verifyErr := console.VerifyAccount(ctx, cfg, fresh)
		if verifyErr != nil {
			result.Reason = "failed"
			if status == "401" {
				result.Reason = "sso_rejected"
			}
			result.Error = grokbase.DiagnosticText(verifyErr.Error(), fresh)
			results = append(results, result)
			continue
		}
		fresh.VerifiedAt = time.Now()
		if err := st.CreateAccount(ctx, fresh); err != nil {
			result.Reason, result.Error = "failed", "保存 Console 账号失败"
			results = append(results, result)
			continue
		}
		existing[console.CredentialKey(fresh)] = fresh
		result.Success, result.BuildAccountID, result.Created = true, fresh.ID, true
		results = append(results, result)
	}
	if err := console.PublishRoutes(ctx, st); err != nil {
		slog.Warn("grok console route publication failed", "error", err)
	}
	ok := 0
	for _, result := range results {
		if result.Success {
			ok++
		}
	}
	util.WriteJSON(w, map[string]any{"status": "success", "total": len(results), "ok": ok, "failed": len(results) - ok, "results": results})
}
