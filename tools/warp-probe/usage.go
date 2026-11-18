package main

import (
	"errors"
	"math"
	"sort"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
)

var errProbeUsage = errors.New("invalid probe usage measurement")

// probeUsage contains request-only observed aggregates. A nil field is unknown,
// not zero; even an explicitly reported zero sets Measured. Aggregates sum only
// present components, so they are not a claim that upstream reporting is complete.
// No model/category identifiers, response text, or raw protobuf data are retained.
type probeUsage struct {
	Measured         bool     `json:"measured"`
	Source           string   `json:"source,omitempty"`
	InputTokens      *uint64  `json:"input_tokens,omitempty"`
	OutputTokens     *uint64  `json:"output_tokens,omitempty"`
	CacheReadTokens  *uint64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *uint64  `json:"cache_write_tokens,omitempty"`
	InferenceCents   *float64 `json:"inference_cents,omitempty"`
	InferenceCredits *float64 `json:"inference_credits,omitempty"`
	PlatformCents    *float64 `json:"platform_cents,omitempty"`
	PlatformCredits  *float64 `json:"platform_credits,omitempty"`
}

// extractProbeUsage prefers request_charges wholesale, including when that
// message is empty. Legacy request_cost and token_usage are considered only if
// request_charges is absent, never added to the newer representation. Conversation
// usage is deliberately never consulted: it can include earlier requests.
func extractProbeUsage(finish *warpapi.ResponseEvent_StreamFinished) (probeUsage, error) {
	var usage probeUsage
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
		for _, category := range probeUsageKeys(categories) {
			charged := categories[category]
			addCost(&usage.PlatformCents, charged.HasPlatformUsageInCents(), charged.GetPlatformUsageInCents())
			addCost(&usage.PlatformCredits, charged.HasPlatformUsageInCredits(), charged.GetPlatformUsageInCredits())
			for _, models := range []map[string]*warpapi.InferenceUsage{
				charged.GetDirectApiInferenceUsage(), charged.GetByokInferenceUsage(), charged.GetCustomEndpointInferenceUsage(),
			} {
				for _, model := range probeUsageKeys(models) {
					inference := models[model]
					count := inference.GetTokenCount()
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
	if !valid {
		return probeUsage{}, errProbeUsage
	}
	if !usage.Measured {
		usage.Source = ""
	}
	return usage, nil
}

func probeUsageKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
