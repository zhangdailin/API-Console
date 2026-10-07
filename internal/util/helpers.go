package util

import (
	"context"
	"crypto/subtle"
	"net"
	"net/url"
	"strings"
	"time"
)

// WithDefaultTimeout creates a context with timeout if not already set
func WithDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// WithAttemptTimeout bounds this operation even when the parent has a longer
// deadline. Zero disables the local bound without discarding parent cancellation.
func WithAttemptTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// UniqueStrings returns a deduplicated slice of non-empty trimmed strings
func UniqueStrings(input []string) []string {
	if len(input) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(input))
	out := make([]string, 0, len(input))
	for _, item := range input {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

// SecureCompare compares secrets without leaking a matching prefix through timing.
func SecureCompare(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// FirstNonEmpty returns the first non-empty (after trimming) value, or "".
// It is the single shared implementation of the firstNonEmpty helper that was
// previously duplicated across packages (grok, api, cmd/server, cline, qoder).
func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// FirstNonEmptyUntrimmed returns the first value that is non-empty after
// trimming, untrimmed. A credential is stored exactly as the upstream sent it:
// trimming an access token would change the bytes the gateway signed.
func FirstNonEmptyUntrimmed(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// FirstNonEmptyURL is FirstNonEmpty for base URLs: a trailing slash would turn
// "https://host" + "/path" into a double slash, which some gateways read as a
// different route.
func FirstNonEmptyURL(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return strings.TrimRight(trimmed, "/")
		}
	}
	return ""
}

// AllowedLoginHost accepts only exact provider/configured hosts and loopback.
func AllowedLoginHost(host, configured string, allowed []string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return false
	}
	if host == "localhost" || host == "::1" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	for _, candidate := range allowed {
		if strings.EqualFold(host, candidate) {
			return true
		}
	}
	configured = strings.TrimSpace(strings.ToLower(configured))
	return configured != "" && host == configured
}

// HostOf returns the host of an absolute URL, or "" when it cannot be parsed.
func HostOf(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// URLPath returns the path of an absolute URL, or "" when it cannot be parsed.
// It keeps a credential out of an error message: only the path is echoed.
func URLPath(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Path
}

// Truncate keeps an upstream message short enough to log without echoing a
// multi-megabyte body.
func Truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

// FirstPositive returns the first strictly positive candidate, or 0.
func FirstPositive(values ...float64) float64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
