package grokbase

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/grok/ratelimit"
	"orchids-api/internal/store"
)

// IsAccountBlockBody accepts only an explicit error code or message saying a
// Grok account is blocked or suspended. A generic 403 is not one.
func IsAccountBlockBody(body []byte) bool {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return IsAccountBlockText(string(body))
	}
	return IsAccountBlockText(strings.Join(JSONStrings(payload), " "))
}

// IsAccountBlockText matches explicit account-block language.
func IsAccountBlockText(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "blocked-user") || strings.Contains(value, "user is blocked")
}

// JSONStrings gathers every string value of a decoded JSON tree, so nested
// error and message fields are inspected, not just the top level.
func JSONStrings(value any) []string {
	var out []string
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		case string:
			out = append(out, typed)
		}
	}
	walk(value)
	return out
}

type rateLimitAccountKey struct{}

// WithRateLimitAccount records which upstream identity a request is rate
// limited under: the account's team, or the account itself.
func WithRateLimitAccount(ctx context.Context, acc *store.Account) context.Context {
	if acc == nil {
		return ctx
	}
	identity := ""
	if acc.TeamID != "" {
		identity = "team:" + acc.TeamID
	}
	if identity == "" && acc.ID != 0 {
		identity = fmt.Sprintf("account:%d", acc.ID)
	}
	return context.WithValue(ctx, rateLimitAccountKey{}, identity)
}

// RateLimitAccount returns the identity WithRateLimitAccount recorded, or "".
func RateLimitAccount(ctx context.Context) string {
	identity, _ := ctx.Value(rateLimitAccountKey{}).(string)
	return identity
}

// RetryAfterSeconds reads the backoff an upstream asked for from its response
// headers: Retry-After, else the reset time of a rate-limit header. 0 means
// none was given.
func RetryAfterSeconds(header http.Header) int {
	if header == nil {
		return 0
	}
	if value := ratelimit.ParseRetryAfter(header.Get("Retry-After"), time.Now()); value > 0 {
		return max(int(value.Round(time.Second)/time.Second), 1)
	}
	if info := ratelimit.ParseHeaders(header); info != nil {
		if seconds := int(time.Until(info.ResetAt).Round(time.Second) / time.Second); seconds > 0 {
			return seconds
		}
	}
	return 0
}
