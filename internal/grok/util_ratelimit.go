package grok

import (
	"net/http"
	"orchids-api/internal/chatwire"
	"strconv"
	"strings"
	"time"
)

func parseRateLimitInfo(headers http.Header) *chatwire.RateLimitInfo {
	if headers == nil {
		return nil
	}
	limitRaw := firstHeaderValue(
		headers,
		"ratelimit-limit",
		"x-ratelimit-limit",
		"x-rate-limit-limit",
		"x-usage-limit",
		"x-ratelimit-limit-requests",
		"x-ratelimit-limit-reqs",
	)
	remainingRaw := firstHeaderValue(
		headers,
		"ratelimit-remaining",
		"x-ratelimit-remaining",
		"x-rate-limit-remaining",
		"x-usage-remaining",
		"x-ratelimit-remaining-requests",
		"x-ratelimit-remaining-reqs",
	)
	resetRaw := firstHeaderValue(
		headers,
		"ratelimit-reset",
		"x-ratelimit-reset",
		"x-rate-limit-reset",
		"x-ratelimit-reset-requests",
	)

	limit, okLimit := parseRateLimitValue(limitRaw)
	remaining, okRemaining := parseRateLimitValue(remainingRaw)
	resetAt := parseRateLimitReset(resetRaw)

	if !okLimit && !okRemaining && resetRaw == "" {
		return nil
	}

	info := &chatwire.RateLimitInfo{
		Limit:        limit,
		HasLimit:     okLimit,
		Remaining:    remaining,
		HasRemaining: okRemaining,
		ResetAt:      resetAt,
		Unit:         "requests",
	}
	return info
}

func firstHeaderValue(headers http.Header, keys ...string) string {
	for _, key := range keys {
		if key == "" {
			continue
		}
		if val := strings.TrimSpace(headers.Get(key)); val != "" {
			return val
		}
	}
	return ""
}

func parseRateLimitValue(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if v, ok := parseNumericToken(raw); ok {
		return v, true
	}
	if token := extractFirstNumberToken(raw); token != "" {
		if v, ok := parseNumericToken(token); ok {
			return v, true
		}
	}
	return 0, false
}

func parseNumericToken(token string) (int64, bool) {
	if token == "" {
		return 0, false
	}
	i := 0
	if token[0] == '+' || token[0] == '-' {
		i = 1
	}
	if i >= len(token) {
		return 0, false
	}

	hasDigit := false
	hasDot := false
	for ; i < len(token); i++ {
		c := token[i]
		if isDigit(c) {
			hasDigit = true
			continue
		}
		if c == '.' && !hasDot {
			hasDot = true
			continue
		}
		return 0, false
	}
	if !hasDigit {
		return 0, false
	}

	if hasDot {
		f, err := strconv.ParseFloat(token, 64)
		if err != nil {
			return 0, false
		}
		return int64(f), true
	}

	v, err := strconv.ParseInt(token, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func extractFirstNumberToken(raw string) string {
	start := -1
	end := -1
	seenDot := false
	seenDigit := false

	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if start < 0 {
			if c == '+' || c == '-' {
				if i+1 < len(raw) && (isDigit(raw[i+1]) || raw[i+1] == '.') {
					start = i
					continue
				}
				continue
			}
			if c == '.' {
				if i+1 < len(raw) && isDigit(raw[i+1]) {
					start = i
					seenDot = true
					continue
				}
				continue
			}
			if isDigit(c) {
				start = i
				seenDigit = true
				continue
			}
			continue
		}

		if isDigit(c) {
			seenDigit = true
			end = i + 1
			continue
		}
		if c == '.' && !seenDot {
			seenDot = true
			if end < 0 {
				end = i + 1
			}
			continue
		}
		break
	}

	if start < 0 || !seenDigit {
		return ""
	}
	if end < 0 {
		end = len(raw)
	}
	return raw[start:end]
}

func parseRateLimitReset(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	if v, ok := parseRateLimitValue(raw); ok {
		// Treat large values as milliseconds.
		if v > 1_000_000_000_000 {
			return time.UnixMilli(v)
		}
		if v > 0 {
			return time.Unix(v, 0)
		}
	}
	return time.Time{}
}
