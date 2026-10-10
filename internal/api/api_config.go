package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"orchids-api/internal/httpclient"
	"strconv"
	"strings"

	"orchids-api/internal/auth"
	"orchids-api/internal/config"
	"orchids-api/internal/middleware"
	"orchids-api/internal/util"
)

func (a *API) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	ip := middleware.ClientIP(r)
	if a.loginLimiter != nil && !a.loginLimiter.Allow(ip) {
		http.Error(w, "Too many login attempts, try again later", http.StatusTooManyRequests)
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	adminUser := a.adminUser
	adminPass := a.adminPass
	if cfg := a.config.Load(); cfg != nil {
		adminUser = cfg.AdminUser
		adminPass = cfg.AdminPass
	}

	if !util.SecureCompare(req.Username, adminUser) || !util.SecureCompare(req.Password, adminPass) {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	token, err := auth.GenerateSessionToken()
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

func (a *API) HandleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session_token")
	if err == nil {
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
		if errors.Is(err, errRetiredCacheSetting) {
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

var errRetiredCacheSetting = errors.New("local token cache settings are retired; cache_strategy still controls upstream cache_control")

func buildConfigFromPatch(r *http.Request, current *config.Config) (*config.Config, error) {
	base := &config.Config{}
	if current != nil {
		copyCfg := *current
		base = &copyCfg
	}

	baseMap, err := configPayload(base)
	if err != nil {
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
	if v, ok := patch["admin_password"]; ok {
		patch["admin_pass"] = v
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
	case "enable_token_refresh", "enable_usage_refresh", "enable_token_count",
		"auto_refresh_token", "kiro_use_builtin_proxy",
		"antigravity_use_builtin_proxy",
		"enable_context_compress", "debug_enabled", "grok_hide_reasoning", "qoder_http2_enabled", "cline_http2_enabled", "workbuddy_http2_enabled":
		if b, ok := parseBoolish(value); ok {
			return b
		}
	case "retry_delay", "request_timeout", "refresh_interval",
		"redis_db", "redis_pool_size", "token_refresh_interval", "load_balancer_cache_ttl", "concurrency_limit",
		"concurrency_timeout", "max_retries", "credential_retries",
		"shared_refusal_wait_budget_ms", "qoder_queue_retry_interval_ms", "upstream_max_conns_per_host", "upstream_max_idle_conns_per_host", "diagnostics_sample_every", "diagnostics_max_concurrent", "stream_flush_interval_ms":
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
	config.ApplyHardcoded(storedCfg)
	if _, err := middleware.NewAnonymousAllowlist(storedCfg.AnonymousAllowIPs); err != nil {
		return fmt.Errorf("anonymous_allow_ips: %w", err)
	}
	if err := config.ValidateProxyConfig(storedCfg); err != nil {
		return fmt.Errorf("proxy config: %w", err)
	}

	data, err := json.Marshal(storedCfg)
	if err != nil {
		return err
	}
	if err := a.store.SetSetting(ctx, "config", string(data)); err != nil {
		return err
	}
	// Runtime configs are immutable after publication. Replacing the pointer is
	// atomic; mutating the previously published object would race with request
	// handlers and background jobs reading its fields.
	a.config.Store(storedCfg)
	a.notifyConfigChanged(storedCfg)
	return nil
}
