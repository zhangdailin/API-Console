package handler

import (
	"encoding/json"
	"math"
	"strconv"
)

// exclusiveInputUsage retains presence independently of the canonical estimates.
// The contract is opt-in, not inferred from cache counts or provider identity.
// Counters are snapshots, never deltas, including repeated terminal events.
type exclusiveInputUsage struct {
	active                         bool
	ordinary, read, write          int
	hasOrdinary, hasRead, hasWrite bool
	hasOutput                      bool
	invalid                        bool
	incompleteContributors         bool
}

func checkedUsageSum(values ...int) (int, bool) {
	total := 0
	for _, value := range values {
		if value < 0 || value > int(^uint(0)>>1)-total {
			return 0, false
		}
		total += value
	}
	return total, true
}

// strictUsageInt rejects malformed, negative, fractional and overflowing values.
// It is used only for the opt-in contract, preserving unmarked provider behavior.
func strictUsageInt(value any) (int, bool) {
	max := uint64(^uint(0) >> 1)
	switch n := value.(type) {
	case int:
		return n, n >= 0
	case int64:
		return int(n), n >= 0 && uint64(n) <= max
	case uint64:
		return int(n), n <= max
	case uint:
		return int(n), uint64(n) <= max
	case float64:
		// The exclusive upper bound also avoids float64 rounding MaxInt up.
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || math.Trunc(n) != n || n > 1<<53-1 || n >= math.Ldexp(1, strconv.IntSize-1) {
			return 0, false
		}
		return int(n), true
	case json.Number:
		parsed, err := strconv.ParseUint(string(n), 10, strconv.IntSize-1)
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

// applyExclusiveUsageLocked applies only observed components. Unknown inputs do
// not become zero and do not turn a local prompt estimate into billable usage.
func (h *streamHandler) applyExclusiveUsageLocked(usage map[string]any) {
	u := &h.exclusiveUsage
	u.active = true
	if incomplete, ok := usage["inputTokensIncomplete"].(bool); ok && incomplete {
		u.incompleteContributors = true
	}
	read := func(keys ...string) (int, bool, bool) {
		for _, key := range keys {
			if value, present := usage[key]; present && value != nil {
				n, valid := strictUsageInt(value)
				return n, true, valid
			}
		}
		return 0, false, true
	}
	apply := func(dst *int, known *bool, keys ...string) {
		value, present, valid := read(keys...)
		if !valid {
			u.invalid = true
			return
		}
		if present {
			*dst, *known = value, true
		}
	}
	apply(&u.ordinary, &u.hasOrdinary, "inputTokens", "input_tokens")
	apply(&u.read, &u.hasRead, "cacheReadTokens", "cache_read_tokens")
	apply(&u.write, &u.hasWrite, "cacheWriteTokens", "cache_creation_input_tokens")
	apply(&h.outputTokens, &u.hasOutput, "outputTokens", "output_tokens")
	if u.hasRead {
		h.cachedInputTokens = u.read
	}
	if u.hasWrite {
		h.cacheWriteTokens = u.write
	}
	if !u.incompleteContributors && u.hasOrdinary && u.hasRead && u.hasWrite {
		input, ok := checkedUsageSum(u.ordinary, u.read, u.write)
		if !ok {
			u.invalid = true
		} else if _, ok := checkedUsageSum(input, h.outputTokens); !ok {
			u.invalid = true
		} else if !u.invalid {
			// Cache writes remain a distinct category, but the current billing
			// API prices them at configured ordinary-input rates (no separate
			// cache-write surcharge). This is not upstream official pricing.
			h.inputTokens = input
		}
	}
	// The legacy flag also controls whether streaming output is estimated.
	// Keep estimating if the exclusive contract omitted its output count.
	h.useUpstreamUsage = u.hasOutput
}

func (h *streamHandler) hasAuthoritativeUsage() bool {
	u := h.exclusiveUsage
	if !u.active {
		return h.useUpstreamUsage
	}
	return !u.invalid && !u.incompleteContributors && u.hasOrdinary && u.hasRead && u.hasWrite && u.hasOutput
}

func (h *streamHandler) totalUsageTokens() int {
	if total, ok := checkedUsageSum(h.inputTokens, h.outputTokens); ok {
		return total
	}
	return 0
}

// messagesUsage is the non-streaming Messages projection. In the exclusive
// contract only actually observed fields are emitted. Unmarked providers retain
// the established projection, including their existing input semantics.
func (h *streamHandler) messagesUsage() map[string]int {
	if !h.exclusiveUsage.active {
		return map[string]int{"input_tokens": h.inputTokens, "output_tokens": h.outputTokens}
	}
	return h.exclusiveWireUsage(false)
}

func (h *streamHandler) appendExclusiveWireUsage(dst []byte, inclusive bool) ([]byte, error) {
	usage := h.exclusiveWireUsage(inclusive)
	if inclusive {
		// Explicit Chat names identify an already-inclusive projection to the
		// adapter. If an input component is missing, omit prompt entirely,
		// rather than allowing native-input fallback to invent a total.
		if input, ok := usage["input_tokens"]; ok {
			usage["prompt_tokens"] = input
			delete(usage, "input_tokens")
		}
		if output, ok := usage["output_tokens"]; ok {
			usage["completion_tokens"] = output
			delete(usage, "output_tokens")
		}
	}
	raw, err := json.Marshal(usage)
	return append(dst, raw...), err
}

// exclusiveWireUsage emits no marker and never invents absent components. Chat
// and its Responses bridge require inclusive input; Messages requires ordinary.
func (h *streamHandler) exclusiveWireUsage(inclusive bool) map[string]int {
	u := h.exclusiveUsage
	out := make(map[string]int, 4)
	if u.hasOutput && !u.invalid {
		out["output_tokens"] = h.outputTokens
	}
	if u.hasRead {
		out["cache_read_input_tokens"] = u.read
	}
	if u.hasWrite {
		out["cache_creation_input_tokens"] = u.write
	}
	if inclusive {
		if !u.invalid && !u.incompleteContributors && u.hasOrdinary && u.hasRead && u.hasWrite {
			out["input_tokens"] = h.inputTokens
		}
	} else if u.hasOrdinary && !u.invalid {
		out["input_tokens"] = u.ordinary
	}
	return out
}
