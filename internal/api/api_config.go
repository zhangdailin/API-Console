package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/auth"
	"orchids-api/internal/config"
	"orchids-api/internal/httpclient"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

var loginAdmission = make(chan struct{}, 32)

func (a *API) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	select {
	case loginAdmission <- struct{}{}:
		defer func() { <-loginAdmission }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Login capacity exhausted", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	adminUser := a.adminUser
	adminPass := a.adminPass
	if cfg := a.config.Load(); cfg != nil {
		adminUser = cfg.AdminUser
		adminPass = cfg.AdminPass
	}

	decoded := false
	valid, allowed, credentialErr := a.loginLimiter.CheckCredentialsWithError(middleware.ClientIP(r), func() (bool, error) {
		if !httpserver.DecodeJSONBodyLimit(w, r, &req, 8<<10) {
			return false, nil
		}
		if len(req.Username) > 256 || len(req.Password) > 4096 {
			http.Error(w, "Login field exceeds the allowed length", http.StatusBadRequest)
			return false, nil
		}
		decoded = true
		var credentialErr error
		adminUser, adminPass, credentialErr = auth.CurrentAdminCredentials(r.Context(), adminUser, adminPass)
		if credentialErr != nil {
			return false, credentialErr
		}
		return strings.TrimSpace(adminPass) != "" && util.SecureCompare(req.Username, adminUser) && util.SecureCompare(req.Password, adminPass), nil
	})
	if !allowed {
		w.Header().Set("Retry-After", "180")
		http.Error(w, "Too many login attempts, try again later", http.StatusTooManyRequests)
		return
	}
	if !decoded {
		return // The bounded decoder already wrote the error.
	}
	if credentialErr != nil {
		http.Error(w, "Admin authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	if !valid {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	token, err := auth.GenerateSessionTokenForPassword(adminPass)
	if err != nil {
		slog.Error("Failed to generate session token", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// NOTE: Do not mark cookies as Secure when served over plain HTTP,
	// otherwise browsers will drop the cookie and the Admin UI will appear unable to log in.
	// When behind a TLS-terminating proxy, honor X-Forwarded-Proto.
	isHTTPS := r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 7,
	})

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// AdminAuthenticationLimiter is shared by login, Basic and header passwords.
func (a *API) AdminAuthenticationLimiter() *middleware.RateLimiter { return a.loginLimiter }

func (a *API) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	cookie, err := r.Cookie("session_token")
	if err == nil {
		if !middleware.CheckAdminCookieWrite(w, r) {
			return
		}
		auth.InvalidateSessionToken(cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (a *API) HandleConfigList(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	data, err := configPayload(a.config.Load())
	if err != nil {
		writeCodeEnvelope(w, 1, nil, "获取配置失败: "+err.Error())
		return
	}
	writeCodeEnvelope(w, 0, data, "")
}

func (a *API) HandleConfigSave(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	patch, err := decodeConfigPatch(r)
	if err != nil {
		if errors.Is(err, errRetiredCacheSetting) || errors.Is(err, errFixedRuntimeSetting) {
			http.Error(w, err.Error(), http.StatusBadRequest)
		} else {
			writeCodeEnvelope(w, 1, nil, "parse request failed: "+err.Error())
		}
		return
	}
	if err := a.mutateSavedConfig(r.Context(), func(base *config.Config) (*config.Config, error) {
		return mergeConfigPatch(base, patch)
	}); err != nil {
		if errors.Is(err, store.ErrConfigConflict) {
			http.Error(w, "Configuration changed; reload before saving", http.StatusConflict)
		} else {
			writeCodeEnvelope(w, 1, nil, "save config failed: "+err.Error())
		}
		return
	}
	writeCodeEnvelope(w, 0, nil, "success")
}

func configPayload(cfg *config.Config) (map[string]any, error) {
	if cfg == nil {
		return map[string]any{}, nil
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if v, ok := payload["admin_pass"]; ok {
		payload["admin_password"] = v
	}
	// The static admin token remains a deployment-level compatibility secret,
	// but it is intentionally absent from configuration management. Browser
	// operators use sessions; inference callers use managed API keys.
	delete(payload, "admin_token")
	if rawProxyURL, ok := payload["proxy_url"].(string); !ok || strings.TrimSpace(rawProxyURL) == "" {
		if proxyURL := httpclient.ProxyURLFromConfig(cfg); proxyURL != nil {
			payload["proxy_url"] = proxyURL.String()
		}
	}
	return payload, nil
}

var errFixedRuntimeSetting = errors.New("generation and authentication policy is fixed")

var errRetiredCacheSetting = errors.New("local token cache settings are retired; cache_strategy still controls upstream cache_control")

func decodeConfigPatch(r *http.Request) (map[string]any, error) {
	patch := map[string]any{}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		return nil, err
	}

	if _, forbidden := patch["admin_token"]; forbidden {
		return nil, fmt.Errorf("admin_token is deployment-managed and cannot be changed through configuration management")
	}
	for _, retired := range []string{"enable_token_cache", "token_cache_ttl", "token_cache_strategy", "cache_token_count", "cache_ttl"} {
		if _, present := patch[retired]; present {
			return nil, fmt.Errorf("%w: %s", errRetiredCacheSetting, retired)
		}
	}
	for _, fixed := range []string{"inference_profile", "anonymous_allow_ips", "request_timeout", "concurrency_timeout", "first_token_timeout_seconds", "shared_stream_idle_timeout_seconds", "workbuddy_default_max_tokens", "shared_refusal_wait_budget_ms", "grok_build_timeout_seconds", "grok_stream_idle_seconds", "grok_build_stream_idle_seconds", "max_retries", "retry_delay", "account_switch_count", "retry_429_interval", "quality_hold_enabled", "quality_hold_max_attempts", "quality_hold_timeout_ms", "quality_hold_on_exhausted", "grok_low_latency"} {
		if _, present := patch[fixed]; present {
			return nil, fmt.Errorf("%w: %s is no longer configurable", errFixedRuntimeSetting, fixed)
		}
	}
	if v, ok := patch["admin_password"]; ok {
		patch["admin_pass"] = v
	}
	delete(patch, "admin_password")
	if v, ok := patch["admin_pass"]; ok {
		if v == nil {
			delete(patch, "admin_pass")
		} else if password, ok := v.(string); ok && strings.TrimSpace(password) == "" {
			delete(patch, "admin_pass")
		}
	}

	return patch, nil
}

func mergeConfigPatch(current *config.Config, patch map[string]any) (*config.Config, error) {
	base := &config.Config{}
	if current != nil {
		copyCfg := *current
		base = &copyCfg
	}

	// Merge into the internal configuration, never the redacted public view.
	rawBase, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	baseMap := map[string]any{}
	if err := json.Unmarshal(rawBase, &baseMap); err != nil {
		return nil, err
	}

	for key, value := range patch {
		baseMap[key] = normalizeConfigPatchValue(key, value)
	}
	if _, ok := patch["proxy_url"]; ok {
		baseMap["proxy_http"] = ""
		baseMap["proxy_https"] = ""
		baseMap["proxy_user"] = ""
		baseMap["proxy_pass"] = ""
	}

	raw, err := json.Marshal(baseMap)
	if err != nil {
		return nil, err
	}
	var newCfg config.Config
	if err := json.Unmarshal(raw, &newCfg); err != nil {
		return nil, err
	}
	return &newCfg, nil
}

func normalizeConfigPatchValue(key string, value any) any {
	if value == nil {
		return nil
	}

	switch key {
	case "debug_enabled", "cline_http2_enabled", "workbuddy_http2_enabled":
		if b, ok := parseBoolish(value); ok {
			return b
		}
	case "redis_db", "redis_pool_size", "concurrency_limit", "upstream_max_conns_per_host", "upstream_max_idle_conns_per_host", "diagnostics_sample_every", "diagnostics_max_concurrent", "stream_flush_interval_ms", "shared_refusal_wait_budget_ms", "account_reservation_wait_budget_ms", "doubao_request_gap_ms":
		if i, ok := parseIntish(value); ok {
			return i
		}
	case "proxy_bypass":
		return normalizeProxyBypassValue(value)
	case "proxy_url":
		return strings.TrimSpace(fmt.Sprint(value))
	}

	return value
}

func parseBoolish(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		s := strings.TrimSpace(strings.ToLower(v))
		switch s {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
	case float64:
		return v != 0, true
	}
	return false, false
}

func parseIntish(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil {
			return n, true
		}
	}
	return 0, false
}

func normalizeProxyBypassValue(value any) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s := strings.TrimSpace(fmt.Sprint(item))
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		lines := strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == ',' })
		out := make([]string, 0, len(lines))
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" {
				out = append(out, line)
			}
		}
		return out
	default:
		return nil
	}
}

func (a *API) readSavedConfig(ctx context.Context, local *config.Config) (*config.Config, string, error) {
	if a.store == nil || local == nil {
		return nil, "", fmt.Errorf("configuration unavailable")
	}
	raw, err := a.store.GetSetting(ctx, "config")
	if err != nil || raw == "" {
		return local, raw, err
	}
	durable := local.Clone()
	if err := json.Unmarshal([]byte(raw), durable); err != nil || strings.TrimSpace(durable.AdminPass) == "" {
		return nil, "", fmt.Errorf("saved configuration is invalid")
	}
	config.ApplyHardcoded(durable)
	return durable, raw, nil
}

// mutateSavedConfig merges a patch with the latest committed snapshot. Redis
// compares that exact preimage so two simultaneous saves cannot drop updates.
func (a *API) mutateSavedConfig(ctx context.Context, patch func(*config.Config) (*config.Config, error)) error {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	local := a.config.Load()
	base, raw, err := a.readSavedConfig(ctx, local)
	if err != nil {
		return err
	}
	session, password := middleware.AdminConfigWriteCredentials(ctx)
	if (password != "" && password != base.AdminPass) ||
		(session == "" && password == "" && (base.AdminPass != local.AdminPass || base.AdminUser != local.AdminUser)) {
		return store.ErrConfigConflict
	}
	next, err := patch(base)
	if err != nil {
		return err
	}
	return a.commitConfig(ctx, raw, session, next)
}

func (a *API) persistConfig(ctx context.Context, current, next *config.Config) error {
	return a.mutateSavedConfig(ctx, func(base *config.Config) (*config.Config, error) {
		if current != nil && !reflect.DeepEqual(base, current) {
			return nil, store.ErrConfigConflict
		}
		return next, nil
	})
}

func (a *API) syncSavedConfig(ctx context.Context) error {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	local := a.config.Load()
	current, raw, err := a.readSavedConfig(ctx, local)
	if err != nil || raw == "" || reflect.DeepEqual(local, current) {
		return err
	}
	if err := config.ValidateProxyConfig(current); err != nil {
		return err
	}
	if err := config.ValidateSavedConfig(current); err != nil {
		return err
	}
	a.publishConfig(current)
	return nil
}

func (a *API) StartConfigSync(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				err := a.syncSavedConfig(readCtx)
				cancel()
				if err != nil && ctx.Err() == nil {
					slog.Warn("could not synchronize runtime configuration", "error", err)
				}
			}
		}
	}()
}

func (a *API) commitConfig(ctx context.Context, previous, session string, newCfg *config.Config) error {
	if newCfg == nil {
		return fmt.Errorf("config is nil")
	}
	if a.store == nil {
		return fmt.Errorf("settings store not configured")
	}

	storedCfg := newCfg.Clone()
	if strings.TrimSpace(storedCfg.AdminPass) == "" {
		return fmt.Errorf("admin password must not be empty")
	}
	config.ApplyHardcoded(storedCfg)
	if err := config.ValidateProxyConfig(storedCfg); err != nil {
		return fmt.Errorf("proxy config: %w", err)
	}
	if err := config.ValidateSavedConfig(storedCfg); err != nil {
		return err
	}

	data, err := json.Marshal(storedCfg)
	if err != nil {
		return err
	}
	if err := a.store.CompareAndSaveConfig(ctx, previous, string(data), session); err != nil {
		return err
	}
	a.publishConfig(storedCfg)
	return nil
}

func (a *API) publishConfig(next *config.Config) {
	current := a.config.Load()
	if current == nil || current.AdminPass != next.AdminPass || current.AdminUser != next.AdminUser {
		auth.ClearLocalSessions()
	}
	a.config.Store(next)
	a.notifyConfigChanged(next)
}
