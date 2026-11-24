package web

import (
	"fmt"
	"orchids-api/internal/grok/ratelimit"
	"sort"
	"strings"
	"time"

	"encoding/json"
)

type rateLimitFieldFamily struct {
	unit          string
	limitKeys     []string
	remainingKeys []string
}

// Each snake-case field is tried before its compact spelling. Keep the
// documented priority when both spellings occur in the same upstream reply.
func rateLimitAliases(keys ...string) []string {
	out := make([]string, 0, 2*len(keys))
	for _, key := range keys {
		out = append(out, key)
		if compact := strings.ReplaceAll(key, "_", ""); compact != key {
			out = append(out, compact)
		}
	}
	return out
}

var rateLimitFamilies = []rateLimitFieldFamily{
	{"tokens", rateLimitAliases("limit_tokens", "max_tokens", "token_limit", "tokens_limit", "total_tokens"),
		rateLimitAliases("remaining_tokens", "tokens_remaining")},
	{"requests", rateLimitAliases("max_queries", "query_limit", "queries_limit", "total_queries", "request_limit", "requests_limit"),
		rateLimitAliases("remaining_queries", "queries_remaining", "remaining_requests")},
	{"", rateLimitAliases("limit", "quota", "quota_limit"), rateLimitAliases("remaining", "quota_remaining")},
}

var rateLimitResetOrder = rateLimitAliases("reset", "reset_at", "reset_at_ms", "reset_time", "reset_timestamp", "next_reset")
var rateLimitResetKeys = stringKeySet(rateLimitResetOrder)
var rateLimitNumericKeys = func() map[string]struct{} {
	out := map[string]struct{}{}
	for _, family := range rateLimitFamilies {
		for _, keys := range [][]string{family.limitKeys, family.remainingKeys} {
			for _, key := range keys {
				out[key] = struct{}{}
			}
		}
	}
	return out
}()

func stringKeySet(keys []string) map[string]struct{} {
	out := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		out[key] = struct{}{}
	}
	return out
}

func parseRateLimitPayload(payload map[string]any) *RateLimitInfo {
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
	if n, ok := parseNumberAny(payload["windowSizeSeconds"]); ok && n > 0 {
		info.WindowSeconds = n
	}
	return info
}

func collectRateLimitPayloadFields(value any, numericFields map[string]int64, resetFields map[string]time.Time) {
	switch v := value.(type) {
	case map[string]any:
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
					if t := ratelimit.ParseReset(fmt.Sprint(item)); !t.IsZero() {
						resetFields[key] = t
					}
				}
			}
		}
		for _, k := range keys {
			collectRateLimitPayloadFields(v[k], numericFields, resetFields)
		}
	case []any:
		for _, item := range v {
			collectRateLimitPayloadFields(item, numericFields, resetFields)
		}
	}
}

func buildRateLimitInfoFromFields(numericFields map[string]int64, resetFields map[string]time.Time) *RateLimitInfo {
	var best *RateLimitInfo
	for _, family := range rateLimitFamilies {
		limit, hasLimit := firstRateLimitNumeric(numericFields, family.limitKeys)
		remaining, hasRemaining := firstRateLimitNumeric(numericFields, family.remainingKeys)
		complete := hasLimit && hasRemaining
		if !complete && (best != nil || (!hasLimit && !hasRemaining)) {
			continue
		}
		best = &RateLimitInfo{Limit: limit, HasLimit: hasLimit, Remaining: remaining, HasRemaining: hasRemaining, Unit: family.unit}
		if complete {
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
	for _, key := range rateLimitResetOrder {
		if t, ok := fields[key]; ok && !t.IsZero() {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseNumberAny(raw any) (int64, bool) {
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
		return ratelimit.ParseValue(v)
	case map[string]any, []any:
		return 0, false
	default:
		return ratelimit.ParseValue(fmt.Sprint(v))
	}
}

func normalizeRateKey(key string) string {
	key = strings.TrimSpace(strings.ToLower(key))
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, " ", "")
	return key
}
