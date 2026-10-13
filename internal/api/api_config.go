package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"orchids-api/internal/httpclient"
	"orchids-api/internal/httpserver"
	"strconv"
	"strings"

	"orchids-api/internal/auth"
	"orchids-api/internal/config"
	"orchids-api/internal/middleware"
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
	a.configMu.Lock()
	defer a.configMu.Unlock()
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	current := a.config.Load()
	newCfg, err := buildConfigFromPatch(r, current)
	if err != nil {
		if errors.Is(err, errRetiredCacheSetting) || errors.Is(err, errFixedRuntimeSetting) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeCodeEnvelope(w, 1, nil, "parse request failed: "+err.Error())
		return
	}
	if err := a.persistConfig(r.Context(), current, newCfg); err != nil {
		writeCodeEnvelope(w, 1, nil, "save config failed: "+err.Error())
		return
	}
	writeCodeEnvelope(w, 0, nil, "success")
}

func configPayload(cfg *config.Config) (map[string]interface{}, error) {
	if cfg == nil {
		return map[string]interface{}{}, nil
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	payload := map[string]interface{}{}
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

func buildConfigFromPatch(r *http.Request, current *config.Config) (*config.Config, error) {
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
	baseMap := map[string]interface{}{}
	if err := json.Unmarshal(rawBase, &baseMap); err != nil {
		return nil, err
	}

	patch := map[string]interface{}{}
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
	for _, fixed := range []string{"anonymous_allow_ips", "request_timeout", "concurrency_timeout", "first_token_timeout_seconds", "shared_stream_idle_timeout_seconds", "workbuddy_default_max_tokens", "shared_refusal_wait_budget_ms", "qoder_queue_wait_budget_ms", "qoder_queue_retry_interval_ms", "grok_build_timeout_seconds", "grok_stream_idle_seconds", "grok_build_stream_idle_seconds", "max_retries", "retry_delay", "account_switch_count", "retry_429_interval", "quality_hold_enabled", "quality_hold_max_attempts", "quality_hold_timeout_ms", "quality_hold_on_exhausted"} {
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

func normalizeConfigPatchValue(key string, value interface{}) interface{} {
	if value == nil {
		return nil
	}

	switch key {
	case "debug_enabled", "qoder_http2_enabled", "cline_http2_enabled", "workbuddy_http2_enabled":
		if b, ok := parseBoolish(value); ok {
			return b
		}
	case "redis_db", "redis_pool_size", "concurrency_limit", "upstream_max_conns_per_host", "upstream_max_idle_conns_per_host", "diagnostics_sample_every", "diagnostics_max_concurrent", "stream_flush_interval_ms":
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

func parseBoolish(value interface{}) (bool, bool) {
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

func parseIntish(value interface{}) (int, bool) {
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

func normalizeProxyBypassValue(value interface{}) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []interface{}:
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

func (a *API) persistConfig(ctx context.Context, current, newCfg *config.Config) error {
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

	data, err := json.Marshal(storedCfg)
	if err != nil {
		return err
	}
	base := current
	if base == nil {
		base = a.config.Load()
	}
	if base == nil {
		return fmt.Errorf("current configuration unavailable")
	}
	if err := a.store.SaveConfigForCredentials(ctx, string(data), base.AdminPass, base.AdminUser); err != nil {
		return err
	}
	if current == nil || current.AdminPass != storedCfg.AdminPass || current.AdminUser != storedCfg.AdminUser {
		auth.ClearLocalSessions()
	}
	// Runtime configs are immutable after publication. Replacing the pointer is
	// atomic; mutating the previously published object would race with request
	// handlers and background jobs reading its fields.
	a.config.Store(storedCfg)
	a.notifyConfigChanged(storedCfg)
	return nil
}
