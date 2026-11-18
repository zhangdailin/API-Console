package warp

import (
	"errors"
	"math"
	"sort"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
)

var errUsage = errors.New("invalid warp usage measurement")

// observedUsage contains request-only observed aggregates. A nil field is unknown,
// not zero; even an explicitly reported zero sets Measured. Aggregates sum only
// present components, so they are not a claim that upstream reporting is complete.
// No model/category identifiers, response text, or raw protobuf data are retained.
type observedUsage struct {
	Measured               bool     `json:"measured"`
	IncompleteContributors bool     `json:"-"`
	Source                 string   `json:"source,omitempty"`
	InputTokens            *uint64  `json:"input_tokens,omitempty"`
	OutputTokens           *uint64  `json:"output_tokens,omitempty"`
	CacheReadTokens        *uint64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens       *uint64  `json:"cache_write_tokens,omitempty"`
	InferenceCents         *float64 `json:"inference_cents,omitempty"`
	InferenceCredits       *float64 `json:"inference_credits,omitempty"`
	PlatformCents          *float64 `json:"platform_cents,omitempty"`
	PlatformCredits        *float64 `json:"platform_credits,omitempty"`
}

// extractUsage prefers request_charges wholesale, including when that
// message is empty. Legacy request_cost and token_usage are considered only if
// request_charges is absent, never added to the newer representation. Conversation
// usage is deliberately never consulted: it can include earlier requests.
func extractUsage(finish *warpapi.ResponseEvent_StreamFinished) (observedUsage, error) {
	var usage observedUsage
	if finish == nil {
		return usage, nil
	}
	valid := true
	addTokens := func(dst **uint64, present bool, value uint64) {
		if !present {
			return
		}
		if *dst == nil {
			*dst = new(uint64)
		}
		if ^uint64(0)-**dst < value {
			valid = false
			return
		}
		**dst += value
		usage.Measured = true
	}
	addCost := func(dst **float64, present bool, value float32) {
		if !present {
			return
		}
		n := float64(value)
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			valid = false
			return
		}
		if *dst == nil {
			*dst = new(float64)
		}
		n += **dst
		if math.IsInf(n, 0) {
			valid = false
			return
		}
		// Canonicalize negative zero before producing JSON.
		if n == 0 {
			n = 0
		}
		**dst = n
		usage.Measured = true
	}
	if finish.HasRequestCharges() {
		usage.Source = "request_charges"
		categories := finish.GetRequestCharges().GetUsageByCategory()
		// Stable traversal avoids nondeterministic floating-point sum order. Map
		// keys are used only for lookup and are never copied into output/errors.
		for _, category := range observedUsageKeys(categories) {
			charged := categories[category]
			addCost(&usage.PlatformCents, charged.HasPlatformUsageInCents(), charged.GetPlatformUsageInCents())
			addCost(&usage.PlatformCredits, charged.HasPlatformUsageInCredits(), charged.GetPlatformUsageInCredits())
			for _, models := range chargedInferenceUsageMaps(charged) {
				for _, model := range observedUsageKeys(models) {
					inference := models[model]
					count := inference.GetTokenCount()
					if count == nil || !count.HasInput() || !count.HasOutput() || !count.HasInputCacheRead() || !count.HasInputCacheWrite() {
						usage.IncompleteContributors = true
					}
					addTokens(&usage.InputTokens, count.HasInput(), count.GetInput())
					addTokens(&usage.OutputTokens, count.HasOutput(), count.GetOutput())
					addTokens(&usage.CacheReadTokens, count.HasInputCacheRead(), count.GetInputCacheRead())
					addTokens(&usage.CacheWriteTokens, count.HasInputCacheWrite(), count.GetInputCacheWrite())
					cost := inference.GetTokenCost()
					addCost(&usage.InferenceCents, cost.HasInputCostInCents(), cost.GetInputCostInCents())
					addCost(&usage.InferenceCents, cost.HasOutputCostInCents(), cost.GetOutputCostInCents())
					addCost(&usage.InferenceCents, cost.HasInputCacheReadCostInCents(), cost.GetInputCacheReadCostInCents())
					addCost(&usage.InferenceCents, cost.HasInputCacheWriteCostInCents(), cost.GetInputCacheWriteCostInCents())
					addCost(&usage.InferenceCredits, cost.HasInputCostInCredits(), cost.GetInputCostInCredits())
					addCost(&usage.InferenceCredits, cost.HasOutputCostInCredits(), cost.GetOutputCostInCredits())
					addCost(&usage.InferenceCredits, cost.HasInputCacheReadCostInCredits(), cost.GetInputCacheReadCostInCredits())
					addCost(&usage.InferenceCredits, cost.HasInputCacheWriteCostInCredits(), cost.GetInputCacheWriteCostInCredits())
					addCost(&usage.InferenceCents, inference.HasWebSearchCostInCents(), inference.GetWebSearchCostInCents())
					addCost(&usage.InferenceCredits, inference.HasWebSearchCostInCredits(), inference.GetWebSearchCostInCredits())
					addCost(&usage.InferenceCents, inference.HasWebFetchCostInCents(), inference.GetWebFetchCostInCents())
					addCost(&usage.InferenceCredits, inference.HasWebFetchCostInCredits(), inference.GetWebFetchCostInCredits())
				}
			}
		}
	} else {
		usage.Source = "legacy_request_usage"
		cost := finish.GetRequestCost()
		addCost(&usage.InferenceCredits, cost.HasExact(), cost.GetExact())
		addCost(&usage.PlatformCredits, cost.HasPlatformCredits(), cost.GetPlatformCredits())
		for _, tokens := range finish.GetTokenUsage() {
			addTokens(&usage.InputTokens, tokens.HasTotalInput(), uint64(tokens.GetTotalInput()))
			addTokens(&usage.OutputTokens, tokens.HasOutput(), uint64(tokens.GetOutput()))
			addTokens(&usage.CacheReadTokens, tokens.HasInputCacheRead(), uint64(tokens.GetInputCacheRead()))
			addTokens(&usage.CacheWriteTokens, tokens.HasInputCacheWrite(), uint64(tokens.GetInputCacheWrite()))
			addCost(&usage.InferenceCents, tokens.HasCostInCents(), tokens.GetCostInCents())
		}
	}
	// Shared protocol and billing counters use signed machine integers. Reject
	// unrepresentable values before content is emitted, never fall back to an
	// estimate for an overflowing provider measurement.
	max := uint64(^uint(0) >> 1)
	for _, value := range []*uint64{usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens} {
		if value != nil && *value > max {
			valid = false
		}
	}
	var knownInput uint64
	for _, value := range []*uint64{usage.InputTokens, usage.CacheReadTokens, usage.CacheWriteTokens} {
		if value == nil {
			continue
		}
		if usage.Source != "request_charges" && value != usage.InputTokens {
			continue
		}
		if *value > max-knownInput {
			valid = false
			break
		}
		knownInput += *value
	}
	if usage.OutputTokens != nil && *usage.OutputTokens > max-knownInput {
		valid = false
	}
	if !valid {
		return observedUsage{}, errUsage
	}
	if !usage.Measured {
		usage.Source = ""
	}
	return usage, nil
}

// usageHasExternalTools reports positive request-scoped evidence of web-tool
// execution, including zero-cost execution reported by a positive count. Absent
// fields and explicitly reported zeros are not evidence of execution. It ignores
// conversation totals and does not validate numeric measurements; extractUsage
// must still be called to reject invalid costs before completing the response.
func usageHasExternalTools(finish *warpapi.ResponseEvent_StreamFinished) bool {
	for _, charged := range finish.GetRequestCharges().GetUsageByCategory() {
		for _, models := range chargedInferenceUsageMaps(charged) {
			for _, inference := range models {
				if inference.GetWebSearchCount() > 0 || inference.GetWebFetchPageCount() > 0 ||
					inference.GetWebSearchCostInCents() > 0 || inference.GetWebSearchCostInCredits() > 0 ||
					inference.GetWebFetchCostInCents() > 0 || inference.GetWebFetchCostInCredits() > 0 {
					return true
				}
			}
		}
	}
	return false
}

// Each map represents a separate billing route. Read each once; legacy request
// usage and conversation charges are not additional copies of these measurements.
func chargedInferenceUsageMaps(charged *warpapi.ChargedUsage) [4]map[string]*warpapi.InferenceUsage {
	return [4]map[string]*warpapi.InferenceUsage{
		charged.GetDirectApiInferenceUsage(),
		charged.GetByokInferenceUsage(),
		charged.GetCustomEndpointInferenceUsage(),
		charged.GetClaudeLinkedBillingInferenceUsage(),
	}
}

func observedUsageKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// streamUsageEvent carries the provider's explicit input-token contract to the
// shared handler. request_charges reports ordinary input separately from cache
// reads and writes; legacy total_input has no such exclusive-input guarantee.
// Keep event() separate: diagnostics must retain the original numeric counts.
func (u observedUsage) streamUsageEvent() map[string]any {
	out := u.event()
	if u.Source == "request_charges" {
		out["inputTokensSemantics"] = "exclusive_cache"
		if u.IncompleteContributors {
			out["inputTokensIncomplete"] = true
		}
	}
	return out
}

// event carries only numeric request-scoped observations, never a credit ceiling.
func (u observedUsage) event() map[string]any {
	out := map[string]any{}
	for key, value := range map[string]*uint64{"inputTokens": u.InputTokens, "outputTokens": u.OutputTokens, "cacheReadTokens": u.CacheReadTokens, "cacheWriteTokens": u.CacheWriteTokens} {
		if value != nil {
			if *value > uint64(^uint(0)>>1) {
				continue
			}
			out[key] = int(*value)
		}
	}
	for key, value := range map[string]*float64{"warpInferenceCredits": u.InferenceCredits, "warpPlatformCredits": u.PlatformCredits, "warpInferenceCents": u.InferenceCents, "warpPlatformCents": u.PlatformCents} {
		if value != nil {
			out[key] = *value
		}
	}
	return out
}
