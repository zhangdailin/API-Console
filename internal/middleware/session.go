package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"encoding/json"

	"orchids-api/internal/auth"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/util"
)

const (
	// An expired or disabled key is reported with the same code an unknown key
	// gets: the caller's remedy is identical (replace the key), and a distinct
	// code only invited clients to special-case it.
	APIKeyDenialExpired     = "invalid_api_key"
	APIKeyDenialRateLimited = "rate_limit_exceeded"
)

type APIKeyPrincipal struct {
	ID            int64
	AllowedModels []string
	MaxConcurrent int
	// BillingLimitUSDTicks is the key's spending cap in USD ticks
	// (1 USD = 10,000,000,000 ticks). Zero means unlimited, which is what a key
	// created before billing existed reports.
	BillingLimitUSDTicks int64
	DenialCode           string
}

type keyConcurrencyEntry struct {
	active int
}

// APIKeyConcurrencyWithTracker uses the deployment-wide Redis tracker when
// available, making the key limit atomic across replicas. Negative tracker IDs
// form a namespace disjoint from positive account IDs.
func APIKeyConcurrencyWithTracker(next http.HandlerFunc, tracker loadbalancer.ConnTracker) http.HandlerFunc {
	var mu sync.Mutex
	entries := make(map[int64]*keyConcurrencyEntry)
	return func(w http.ResponseWriter, r *http.Request) {
		principal, _ := r.Context().Value(apiKeyPrincipalContextKey{}).(*APIKeyPrincipal)
		if principal == nil || principal.ID == 0 || principal.MaxConcurrent <= 0 {
			next(w, r)
			return
		}
		if tracker != nil {
			trackerID := -principal.ID
			acquired := false
			if limiter, ok := tracker.(loadbalancer.LimitedConnTracker); ok {
				acquired = limiter.TryAcquire(trackerID, int64(principal.MaxConcurrent))
			} else if tracker.GetCount(trackerID) < int64(principal.MaxConcurrent) {
				tracker.Acquire(trackerID)
				acquired = true
			}
			if !acquired {
				w.Header().Set("Retry-After", "1")
				writeAPIKeyError(w, http.StatusTooManyRequests, "API key concurrency limit exceeded", "concurrency_limit_exceeded")
				return
			}
			defer tracker.Release(trackerID)
			next(w, r)
			return
		}
		mu.Lock()
		entry := entries[principal.ID]
		if entry == nil {
			entry = &keyConcurrencyEntry{}
			entries[principal.ID] = entry
		}
		if entry.active >= principal.MaxConcurrent {
			mu.Unlock()
			w.Header().Set("Retry-After", "1")
			writeAPIKeyError(w, http.StatusTooManyRequests, "API key concurrency limit exceeded", "concurrency_limit_exceeded")
			return
		}
		entry.active++
		mu.Unlock()
		defer func() {
			mu.Lock()
			entry.active--
			if entry.active <= 0 {
				delete(entries, principal.ID)
			}
			mu.Unlock()
		}()
		next(w, r)
	}
}

type APIKeyValidator func(context.Context, string) (*APIKeyPrincipal, error)

type apiKeyFingerprintContextKey struct{}
type apiKeyPrincipalContextKey struct{}

// APIKeyFingerprint returns a stable, non-secret identity for the validated
// client key. Stateful inference resources use it to enforce ownership.
func APIKeyFingerprint(ctx context.Context) string {
	value, _ := ctx.Value(apiKeyFingerprintContextKey{}).(string)
	return value
}

// APIKeyID returns the non-secret database identity of the validated client
// key for request audit and usage-ledger attribution.
func APIKeyID(ctx context.Context) int64 {
	principal, _ := ctx.Value(apiKeyPrincipalContextKey{}).(*APIKeyPrincipal)
	if principal == nil {
		return 0
	}
	return principal.ID
}

// APIKeyAllowsModel checks the exact, case-insensitive model allowlist attached
// by APIKeyAuth. An absent or empty allowlist is unrestricted for compatibility.
func APIKeyAllowsModel(ctx context.Context, model string) bool {
	principal, _ := ctx.Value(apiKeyPrincipalContextKey{}).(*APIKeyPrincipal)
	if principal == nil || len(principal.AllowedModels) == 0 {
		return true
	}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, allowed := range principal.AllowedModels {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed == "*" || (model != "" && allowed == model) {
			return true
		}
	}
	return false
}

// APIKeyAuth requires a managed key on every model and inference request.
// OpenAI Bearer auth and Anthropic x-api-key headers share the same validator.
func APIKeyAuth(validate APIKeyValidator, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := apiKeyToken(r)
		if token == "" {
			writeAPIKeyError(w, http.StatusUnauthorized, "Missing API key", "invalid_api_key")
			return
		}
		principal, err := validate(r.Context(), token)
		if err != nil {
			writeAPIKeyError(w, http.StatusServiceUnavailable, "API key validation unavailable", "auth_unavailable")
			return
		}
		if principal == nil {
			writeAPIKeyError(w, http.StatusUnauthorized, "Invalid API key", "invalid_api_key")
			return
		}
		switch principal.DenialCode {
		case APIKeyDenialExpired:
			writeAPIKeyError(w, http.StatusUnauthorized, "API key has expired", APIKeyDenialExpired)
			return
		case APIKeyDenialRateLimited:
			w.Header().Set("Retry-After", "60")
			writeAPIKeyError(w, http.StatusTooManyRequests, "API key rate limit exceeded", APIKeyDenialRateLimited)
			return
		}
		digest := sha256.Sum256([]byte(token))
		ctx := context.WithValue(r.Context(), apiKeyFingerprintContextKey{}, hex.EncodeToString(digest[:]))
		ctx = context.WithValue(ctx, apiKeyPrincipalContextKey{}, principal)
		next(w, r.WithContext(ctx))
	}
}

func writeAPIKeyError(w http.ResponseWriter, status int, message, code string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	errorType := "authentication_error"
	switch status {
	case http.StatusTooManyRequests:
		errorType = "rate_limit_error"
	case http.StatusPaymentRequired:
		// A spent budget is a quota problem, not a credential one. The code stays
		// explicit so a client can distinguish it from a malformed key.
		errorType = "insufficient_quota"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
			"code":    code,
		},
	})
}

func bearerToken(r *http.Request) string {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		return ""
	}
	parts := strings.Fields(authHeader)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func apiKeyToken(r *http.Request) string {
	if token := bearerToken(r); token != "" {
		return token
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

func SessionAuthDynamic(credentials func() (adminPass, adminToken string), next http.HandlerFunc, limiters ...*RateLimiter) http.HandlerFunc {
	limiter := &RateLimiter{maxAttempts: 5, window: 15 * time.Minute}
	if len(limiters) > 0 && limiters[0] != nil {
		limiter = limiters[0]
	}
	// Every admin entrance — browser session, static token, basic auth — passes
	// through here, which makes it the one place that can journal administrative
	// changes without each handler remembering to.
	audited := adminSessionAudit(next)
	return func(w http.ResponseWriter, r *http.Request) {
		adminPass, adminToken := credentials()

		cookie, err := r.Cookie("session_token")
		if err == nil && auth.ValidateSessionToken(cookie.Value) {
			if !CheckAdminCookieWrite(w, r) {
				return
			}
			audited(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		adminHeader := r.Header.Get("X-Admin-Token")
		matchesHeader := func(secret string) bool {
			return strings.TrimSpace(secret) != "" && (util.SecureCompare(authHeader, "Bearer "+secret) ||
				util.SecureCompare(authHeader, secret) || util.SecureCompare(adminHeader, secret))
		}
		if matchesHeader(adminToken) {
			audited(w, r)
			return
		}
		if authHeader == "" && adminHeader == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		valid, allowed, credentialErr := limiter.CheckCredentialsWithError(ClientIP(r), func() (bool, error) {
			var credentialErr error
			_, adminPass, credentialErr = auth.CurrentAdminCredentials(r.Context(), "", adminPass)
			if credentialErr != nil {
				return false, credentialErr
			}
			_, pass, ok := r.BasicAuth()
			return matchesHeader(adminPass) || (ok && strings.TrimSpace(adminPass) != "" && util.SecureCompare(pass, adminPass)), nil
		})
		if credentialErr != nil {
			http.Error(w, "Admin authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		if !allowed {
			w.Header().Set("Retry-After", "180")
			http.Error(w, "Too many login attempts, try again later", http.StatusTooManyRequests)
			return
		}
		if valid {
			audited(w, r)
			return
		}

		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}
}
