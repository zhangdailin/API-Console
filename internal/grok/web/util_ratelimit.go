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
	if n, ok := parseNumberAny(payload["windowSizeSeconds"]); ok && n > 0 {
		info.WindowSeconds = n
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
					if t := ratelimit.ParseReset(fmt.Sprint(item)); !t.IsZero() {
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
		return ratelimit.ParseValue(v)
	case map[string]interface{}, []interface{}:
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
