package web

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"encoding/json"
)

type rateLimitFieldFamily struct {
	unit          string
	limitKeys     []string
	remainingKeys []string
}

func buildRateLimitNumericKeySet(families []rateLimitFieldFamily) map[string]struct{} {
	out := make(map[string]struct{})
	for _, family := range families {
		for _, key := range family.limitKeys {
			out[key] = struct{}{}
		}
		for _, key := range family.remainingKeys {
			out[key] = struct{}{}
		}
	}
	return out
}

func parseRateLimitInfo(headers http.Header) *RateLimitInfo {
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

	info := &RateLimitInfo{
		Limit:        limit,
		HasLimit:     okLimit,
		Remaining:    remaining,
		HasRemaining: okRemaining,
		ResetAt:      resetAt,
		Unit:         "requests",
	}
	return info
}

func parseRateLimitPayload(payload map[string]interface{}) *RateLimitInfo {
	if payload == nil {
		return nil
	}

	numericFields := make(map[string]int64)
	resetFields := make(map[string]time.Time)
	collectRateLimitPayloadFields(payload, numericFields, resetFields)

	info := buildRateLimitInfoFromFields(numericFields, resetFields)
	if info == nil {
		return nil
	}
	return info
}

func collectRateLimitPayloadFields(value interface{}, numericFields map[string]int64, resetFields map[string]time.Time) {
	switch v := value.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			item := v[k]
			key := normalizeRateKey(k)
			if _, ok := rateLimitNumericKeys[key]; ok {
				if _, seen := numericFields[key]; !seen {
					if n, ok := parseNumberAny(item); ok {
						numericFields[key] = n
					}
				}
			}
			if _, ok := rateLimitResetKeys[key]; ok {
				if _, seen := resetFields[key]; !seen {
					if t := parseRateLimitReset(fmt.Sprint(item)); !t.IsZero() {
						resetFields[key] = t
					}
				}
			}
		}
		for _, k := range keys {
			collectRateLimitPayloadFields(v[k], numericFields, resetFields)
		}
	case []interface{}:
		for _, item := range v {
			collectRateLimitPayloadFields(item, numericFields, resetFields)
		}
	}
}

func buildRateLimitInfoFromFields(numericFields map[string]int64, resetFields map[string]time.Time) *RateLimitInfo {
	var (
		best         *RateLimitInfo
		bestRank     = -1
		bestComplete bool
	)

	for idx, family := range rateLimitFamilies {
		limit, hasLimit := firstRateLimitNumeric(numericFields, family.limitKeys)
		remaining, hasRemaining := firstRateLimitNumeric(numericFields, family.remainingKeys)
		if !hasLimit && !hasRemaining {
			continue
		}
		complete := hasLimit && hasRemaining
		if best != nil {
			if bestComplete && !complete {
				continue
			}
			if bestComplete == complete && bestRank <= idx {
				continue
			}
		}
		best = &RateLimitInfo{
			Limit:        limit,
			HasLimit:     hasLimit,
			Remaining:    remaining,
			HasRemaining: hasRemaining,
			Unit:         family.unit,
		}
		bestRank = idx
		bestComplete = complete
		if complete && idx == 0 {
			break
		}
	}

	resetAt, hasReset := firstRateLimitReset(resetFields)
	if best == nil {
		if !hasReset {
			return nil
		}
		return &RateLimitInfo{ResetAt: resetAt}
	}
	if hasReset {
		best.ResetAt = resetAt
	}
	return best
}

func firstRateLimitNumeric(fields map[string]int64, keys []string) (int64, bool) {
	for _, key := range keys {
		if v, ok := fields[key]; ok {
			return v, true
		}
	}
	return 0, false
}

func firstRateLimitReset(fields map[string]time.Time) (time.Time, bool) {
	for _, key := range []string{
		"reset",
		"reset_at",
		"resetat",
		"reset_at_ms",
		"resetatms",
		"reset_time",
		"resettime",
		"reset_timestamp",
		"resettimestamp",
		"next_reset",
		"nextreset",
	} {
		if t, ok := fields[key]; ok && !t.IsZero() {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseNumberAny(raw interface{}) (int64, bool) {
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int8:
		return int64(v), true
	case int16:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		return int64(v), true
	case uint8:
		return int64(v), true
	case uint16:
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint64:
		if v > uint64(1<<63-1) {
			return 0, false
		}
		return int64(v), true
	case float64:
		return int64(v), true
	case float32:
		return int64(v), true
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, true
		}
		if f, err := v.Float64(); err == nil {
			return int64(f), true
		}
		return 0, false
	case string:
		return parseRateLimitValue(v)
	case map[string]interface{}, []interface{}:
		return 0, false
	default:
		return parseRateLimitValue(fmt.Sprint(v))
	}
}

func normalizeRateKey(key string) string {
	key = strings.TrimSpace(strings.ToLower(key))
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, " ", "")
	return key
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
