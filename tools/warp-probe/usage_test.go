package main

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
)

func usageTestPtr[T any](value T) *T { return &value }

func usageTestFinish(charged *warpapi.ChargedUsage) *warpapi.ResponseEvent_StreamFinished {
	return warpapi.ResponseEvent_StreamFinished_builder{
		RequestCharges: warpapi.RequestCharges_builder{UsageByCategory: map[string]*warpapi.ChargedUsage{"private-category": charged}}.Build(),
	}.Build()
}

func TestExtractProbeUsageUnknown(t *testing.T) {
	conversation := warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata_builder{
		CreditsSpent:         usageTestPtr(float32(100)),
		PlatformCreditsSpent: usageTestPtr(float32(200)),
		TotalInputTokens:     usageTestPtr(uint32(300)),
		TotalCharges: warpapi.RequestCharges_builder{UsageByCategory: map[string]*warpapi.ChargedUsage{
			"private-conversation": warpapi.ChargedUsage_builder{PlatformUsageInCredits: usageTestPtr(float32(400))}.Build(),
		}}.Build(),
	}.Build()
	cases := map[string]*warpapi.ResponseEvent_StreamFinished{
		"nil":                 nil,
		"empty":               warpapi.ResponseEvent_StreamFinished_builder{}.Build(),
		"conversation only":   warpapi.ResponseEvent_StreamFinished_builder{ConversationUsageMetadata: conversation}.Build(),
		"empty new envelope":  usageTestFinish(warpapi.ChargedUsage_builder{}.Build()),
		"nil new map entry":   usageTestFinish(nil),
		"empty legacy values": warpapi.ResponseEvent_StreamFinished_builder{RequestCost: warpapi.ResponseEvent_StreamFinished_RequestCost_builder{}.Build(), TokenUsage: []*warpapi.ResponseEvent_StreamFinished_TokenUsage{nil, warpapi.ResponseEvent_StreamFinished_TokenUsage_builder{}.Build()}}.Build(),
		"empty nested values": usageTestFinish(warpapi.ChargedUsage_builder{DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{
			"private-model": warpapi.InferenceUsage_builder{TokenCount: warpapi.TokenCount_builder{}.Build(), TokenCost: warpapi.TokenCost_builder{}.Build()}.Build(),
			"nil-model":     nil,
		}}.Build()),
	}
	for name, finish := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := extractProbeUsage(finish)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(got)
			if err != nil || string(encoded) != `{"measured":false}` {
				t.Fatalf("expected unknown usage, got %s, error %v", encoded, err)
			}
		})
	}
}

func TestExtractProbeUsageExplicitZero(t *testing.T) {
	finish := usageTestFinish(warpapi.ChargedUsage_builder{
		PlatformUsageInCredits: usageTestPtr(float32(0)),
		DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{"private-model": warpapi.InferenceUsage_builder{
			TokenCount: warpapi.TokenCount_builder{Input: usageTestPtr(uint64(0)), InputCacheRead: usageTestPtr(uint64(0))}.Build(),
			TokenCost:  warpapi.TokenCost_builder{OutputCostInCents: usageTestPtr(float32(0))}.Build(),
		}.Build()},
	}.Build())
	got, err := extractProbeUsage(finish)
	if err != nil || !got.Measured || got.Source != "request_charges" {
		t.Fatalf("unexpected extraction: %+v, %v", got, err)
	}
	encoded, err := json.Marshal(got)
	if err != nil || string(encoded) != `{"measured":true,"source":"request_charges","input_tokens":0,"cache_read_tokens":0,"inference_cents":0,"platform_credits":0}` {
		t.Fatalf("presence not preserved: %s, %v", encoded, err)
	}
	if got.OutputTokens != nil || got.InferenceCredits != nil || got.PlatformCents != nil {
		t.Fatal("absent fields were converted into zero")
	}
}

func TestExtractProbeUsageAggregates(t *testing.T) {
	inference := func(input, output, read, write uint64, cents, credits float32) *warpapi.InferenceUsage {
		return warpapi.InferenceUsage_builder{
			TokenCount: warpapi.TokenCount_builder{Input: &input, Output: &output, InputCacheRead: &read, InputCacheWrite: &write}.Build(),
			TokenCost: warpapi.TokenCost_builder{
				InputCostInCents: &cents, OutputCostInCents: &cents, InputCacheReadCostInCents: &cents, InputCacheWriteCostInCents: &cents,
				InputCostInCredits: &credits, OutputCostInCredits: &credits, InputCacheReadCostInCredits: &credits, InputCacheWriteCostInCredits: &credits,
			}.Build(),
			WebSearchCostInCents: &cents, WebSearchCostInCredits: &credits,
		}.Build()
	}
	finish := warpapi.ResponseEvent_StreamFinished_builder{
		RequestCharges: warpapi.RequestCharges_builder{UsageByCategory: map[string]*warpapi.ChargedUsage{
			"private-category-a": warpapi.ChargedUsage_builder{
				DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{"private-model-a": inference(10, 20, 3, 4, 1, 2), "private-model-b": inference(10, 20, 3, 4, 1, 2)},
				ByokInferenceUsage:      map[string]*warpapi.InferenceUsage{"private-model-a": inference(10, 20, 3, 4, 1, 2)},
				PlatformUsageInCents:    usageTestPtr(float32(0.5)), PlatformUsageInCredits: usageTestPtr(float32(0.25)),
			}.Build(),
			"private-category-b": warpapi.ChargedUsage_builder{
				CustomEndpointInferenceUsage: map[string]*warpapi.InferenceUsage{"private-model-c": inference(10, 20, 3, 4, 1, 2)},
				PlatformUsageInCents:         usageTestPtr(float32(1.5)), PlatformUsageInCredits: usageTestPtr(float32(0.75)),
			}.Build(),
		}}.Build(),
		// Both are obsolete representations of the same request, not additive.
		RequestCost: warpapi.ResponseEvent_StreamFinished_RequestCost_builder{Exact: usageTestPtr(float32(999))}.Build(),
		TokenUsage: []*warpapi.ResponseEvent_StreamFinished_TokenUsage{
			warpapi.ResponseEvent_StreamFinished_TokenUsage_builder{TotalInput: usageTestPtr(uint32(999)), CostInCents: usageTestPtr(float32(999))}.Build(),
		},
	}.Build()
	got, err := extractProbeUsage(finish)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Measured || got.InputTokens == nil || *got.InputTokens != 40 || got.OutputTokens == nil || *got.OutputTokens != 80 || got.CacheReadTokens == nil || *got.CacheReadTokens != 12 || got.CacheWriteTokens == nil || *got.CacheWriteTokens != 16 {
		t.Fatalf("wrong token aggregates: %+v", got)
	}
	if got.InferenceCents == nil || *got.InferenceCents != 20 || got.InferenceCredits == nil || *got.InferenceCredits != 40 || got.PlatformCents == nil || *got.PlatformCents != 2 || got.PlatformCredits == nil || *got.PlatformCredits != 1 {
		t.Fatalf("wrong cost aggregates: %+v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil || strings.Contains(string(encoded), "private-") {
		t.Fatalf("identifier leaked: %s, %v", encoded, err)
	}
}

func TestExtractProbeUsageLegacyFallback(t *testing.T) {
	finish := warpapi.ResponseEvent_StreamFinished_builder{
		RequestCost: warpapi.ResponseEvent_StreamFinished_RequestCost_builder{Exact: usageTestPtr(float32(0)), PlatformCredits: usageTestPtr(float32(2.5))}.Build(),
		TokenUsage: []*warpapi.ResponseEvent_StreamFinished_TokenUsage{
			warpapi.ResponseEvent_StreamFinished_TokenUsage_builder{TotalInput: usageTestPtr(uint32(5)), Output: usageTestPtr(uint32(2)), InputCacheRead: usageTestPtr(uint32(3)), InputCacheWrite: usageTestPtr(uint32(1)), CostInCents: usageTestPtr(float32(0.5))}.Build(),
			warpapi.ResponseEvent_StreamFinished_TokenUsage_builder{TotalInput: usageTestPtr(uint32(7)), CostInCents: usageTestPtr(float32(1.5))}.Build(),
		},
	}.Build()
	got, err := extractProbeUsage(finish)
	if err != nil || !got.Measured || got.Source != "legacy_request_usage" || got.InputTokens == nil || *got.InputTokens != 12 || got.OutputTokens == nil || *got.OutputTokens != 2 || got.CacheReadTokens == nil || *got.CacheReadTokens != 3 || got.CacheWriteTokens == nil || *got.CacheWriteTokens != 1 || got.InferenceCents == nil || *got.InferenceCents != 2 || got.InferenceCredits == nil || *got.InferenceCredits != 0 || got.PlatformCredits == nil || *got.PlatformCredits != 2.5 || got.PlatformCents != nil {
		t.Fatalf("wrong legacy usage: %+v, %v", got, err)
	}
	finish.SetRequestCharges(warpapi.RequestCharges_builder{}.Build())
	got, err = extractProbeUsage(finish)
	if err != nil || got.Measured || got.InferenceCredits != nil || got.InputTokens != nil {
		t.Fatalf("empty new envelope fell back to obsolete usage: %+v, %v", got, err)
	}
}

func TestExtractProbeUsageInvalidCost(t *testing.T) {
	for name, value := range map[string]float32{"negative": -1, "nan": float32(math.NaN()), "positive infinity": float32(math.Inf(1)), "negative infinity": float32(math.Inf(-1))} {
		t.Run(name, func(t *testing.T) {
			cases := []*warpapi.ResponseEvent_StreamFinished{
				usageTestFinish(warpapi.ChargedUsage_builder{PlatformUsageInCents: &value}.Build()),
				usageTestFinish(warpapi.ChargedUsage_builder{PlatformUsageInCredits: &value}.Build()),
				usageTestFinish(warpapi.ChargedUsage_builder{DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{
					"private-model": warpapi.InferenceUsage_builder{TokenCost: warpapi.TokenCost_builder{InputCostInCents: &value}.Build()}.Build(),
				}}.Build()),
				usageTestFinish(warpapi.ChargedUsage_builder{ByokInferenceUsage: map[string]*warpapi.InferenceUsage{
					"private-model": warpapi.InferenceUsage_builder{WebSearchCostInCredits: &value}.Build(),
				}}.Build()),
				warpapi.ResponseEvent_StreamFinished_builder{RequestCost: warpapi.ResponseEvent_StreamFinished_RequestCost_builder{Exact: &value}.Build()}.Build(),
				warpapi.ResponseEvent_StreamFinished_builder{RequestCost: warpapi.ResponseEvent_StreamFinished_RequestCost_builder{PlatformCredits: &value}.Build()}.Build(),
				warpapi.ResponseEvent_StreamFinished_builder{TokenUsage: []*warpapi.ResponseEvent_StreamFinished_TokenUsage{warpapi.ResponseEvent_StreamFinished_TokenUsage_builder{CostInCents: &value}.Build()}}.Build(),
			}
			for _, set := range []func(*warpapi.TokenCost, float32){
				(*warpapi.TokenCost).SetInputCostInCents,
				(*warpapi.TokenCost).SetOutputCostInCents,
				(*warpapi.TokenCost).SetInputCacheReadCostInCents,
				(*warpapi.TokenCost).SetInputCacheWriteCostInCents,
				(*warpapi.TokenCost).SetInputCostInCredits,
				(*warpapi.TokenCost).SetOutputCostInCredits,
				(*warpapi.TokenCost).SetInputCacheReadCostInCredits,
				(*warpapi.TokenCost).SetInputCacheWriteCostInCredits,
			} {
				cost := warpapi.TokenCost_builder{}.Build()
				set(cost, value)
				cases = append(cases, usageTestFinish(warpapi.ChargedUsage_builder{CustomEndpointInferenceUsage: map[string]*warpapi.InferenceUsage{
					"private-model": warpapi.InferenceUsage_builder{TokenCost: cost}.Build(),
				}}.Build()))
			}
			cases = append(cases, usageTestFinish(warpapi.ChargedUsage_builder{DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{
				"private-model": warpapi.InferenceUsage_builder{WebSearchCostInCents: &value}.Build(),
			}}.Build()))
			for i, finish := range cases {
				got, err := extractProbeUsage(finish)
				if !errors.Is(err, errProbeUsage) || got != (probeUsage{}) || err.Error() != "invalid probe usage measurement" {
					t.Fatalf("case %d: expected sanitized validation error, got %+v, %v", i, got, err)
				}
			}
		})
	}
}

func TestExtractProbeUsageTokenOverflow(t *testing.T) {
	finish := usageTestFinish(warpapi.ChargedUsage_builder{DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{
		"a": warpapi.InferenceUsage_builder{TokenCount: warpapi.TokenCount_builder{Input: usageTestPtr(^uint64(0))}.Build()}.Build(),
		"b": warpapi.InferenceUsage_builder{TokenCount: warpapi.TokenCount_builder{Input: usageTestPtr(uint64(1))}.Build()}.Build(),
	}}.Build())
	got, err := extractProbeUsage(finish)
	if !errors.Is(err, errProbeUsage) || got != (probeUsage{}) {
		t.Fatalf("overflow was not rejected: %+v, %v", got, err)
	}
}
