package warp

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
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
		"linked counts only": usageTestFinish(warpapi.ChargedUsage_builder{ClaudeLinkedBillingInferenceUsage: map[string]*warpapi.InferenceUsage{
			"private-model": warpapi.InferenceUsage_builder{WebFetchPageCount: usageTestPtr(uint32(1))}.Build(),
			"nil-model":     nil,
		}}.Build()),
		"empty nested values": usageTestFinish(warpapi.ChargedUsage_builder{DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{
			"private-model": warpapi.InferenceUsage_builder{TokenCount: warpapi.TokenCount_builder{}.Build(), TokenCost: warpapi.TokenCost_builder{}.Build()}.Build(),
			"nil-model":     nil,
		}}.Build()),
	}
	for name, finish := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := extractUsage(finish)
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
	got, err := extractUsage(finish)
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
			WebFetchCostInCents: &cents, WebFetchCostInCredits: &credits,
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
				CustomEndpointInferenceUsage:      map[string]*warpapi.InferenceUsage{"private-model-c": inference(10, 20, 3, 4, 1, 2)},
				ClaudeLinkedBillingInferenceUsage: map[string]*warpapi.InferenceUsage{"private-model-a": inference(10, 20, 3, 4, 0, 0)},
				PlatformUsageInCents:              usageTestPtr(float32(1.5)), PlatformUsageInCredits: usageTestPtr(float32(0.75)),
			}.Build(),
		}}.Build(),
		// Both are obsolete representations of the same request, not additive.
		RequestCost: warpapi.ResponseEvent_StreamFinished_RequestCost_builder{Exact: usageTestPtr(float32(999))}.Build(),
		TokenUsage: []*warpapi.ResponseEvent_StreamFinished_TokenUsage{
			warpapi.ResponseEvent_StreamFinished_TokenUsage_builder{TotalInput: usageTestPtr(uint32(999)), CostInCents: usageTestPtr(float32(999))}.Build(),
		},
	}.Build()
	got, err := extractUsage(finish)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Measured || got.InputTokens == nil || *got.InputTokens != 50 || got.OutputTokens == nil || *got.OutputTokens != 100 || got.CacheReadTokens == nil || *got.CacheReadTokens != 15 || got.CacheWriteTokens == nil || *got.CacheWriteTokens != 20 {
		t.Fatalf("wrong token aggregates: %+v", got)
	}
	if got.InferenceCents == nil || *got.InferenceCents != 24 || got.InferenceCredits == nil || *got.InferenceCredits != 48 || got.PlatformCents == nil || *got.PlatformCents != 2 || got.PlatformCredits == nil || *got.PlatformCredits != 1 {
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
	got, err := extractUsage(finish)
	if err != nil || !got.Measured || got.Source != "legacy_request_usage" || got.InputTokens == nil || *got.InputTokens != 12 || got.OutputTokens == nil || *got.OutputTokens != 2 || got.CacheReadTokens == nil || *got.CacheReadTokens != 3 || got.CacheWriteTokens == nil || *got.CacheWriteTokens != 1 || got.InferenceCents == nil || *got.InferenceCents != 2 || got.InferenceCredits == nil || *got.InferenceCredits != 0 || got.PlatformCredits == nil || *got.PlatformCredits != 2.5 || got.PlatformCents != nil {
		t.Fatalf("wrong legacy usage: %+v, %v", got, err)
	}
	finish.SetRequestCharges(warpapi.RequestCharges_builder{}.Build())
	got, err = extractUsage(finish)
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
				got, err := extractUsage(finish)
				if !errors.Is(err, errUsage) || got != (observedUsage{}) || err.Error() != "invalid warp usage measurement" {
					t.Fatalf("case %d: expected sanitized validation error, got %+v, %v", i, got, err)
				}
			}
		})
	}
}

func TestExtractProbeUsageAllInferenceCostLeavesInvalid(t *testing.T) {
	for mapName, setMap := range usageTestInferenceMapSetters() {
		for leafName, setLeaf := range usageTestInferenceCostSetters() {
			for valueName, value := range map[string]float32{
				"negative": -1, "nan": float32(math.NaN()),
				"positive infinity": float32(math.Inf(1)), "negative infinity": float32(math.Inf(-1)),
			} {
				t.Run(mapName+"/"+leafName+"/"+valueName, func(t *testing.T) {
					inference := warpapi.InferenceUsage_builder{}.Build()
					setLeaf(inference, value)
					charged := warpapi.ChargedUsage_builder{}.Build()
					setMap(charged, map[string]*warpapi.InferenceUsage{"private-model": inference})
					got, err := extractUsage(usageTestFinish(charged))
					if !errors.Is(err, errUsage) || got != (observedUsage{}) || err.Error() != "invalid warp usage measurement" {
						t.Fatalf("expected sanitized validation error, got %+v, %v", got, err)
					}
				})
			}
		}
	}
}

func usageTestInferenceMapSetters() map[string]func(*warpapi.ChargedUsage, map[string]*warpapi.InferenceUsage) {
	return map[string]func(*warpapi.ChargedUsage, map[string]*warpapi.InferenceUsage){
		"direct": (*warpapi.ChargedUsage).SetDirectApiInferenceUsage,
		"byok":   (*warpapi.ChargedUsage).SetByokInferenceUsage,
		"custom": (*warpapi.ChargedUsage).SetCustomEndpointInferenceUsage,
		"linked": (*warpapi.ChargedUsage).SetClaudeLinkedBillingInferenceUsage,
	}
}

func usageTestInferenceCostSetters() map[string]func(*warpapi.InferenceUsage, float32) {
	setters := map[string]func(*warpapi.InferenceUsage, float32){
		"search cents":   (*warpapi.InferenceUsage).SetWebSearchCostInCents,
		"search credits": (*warpapi.InferenceUsage).SetWebSearchCostInCredits,
		"fetch cents":    (*warpapi.InferenceUsage).SetWebFetchCostInCents,
		"fetch credits":  (*warpapi.InferenceUsage).SetWebFetchCostInCredits,
	}
	for name, set := range map[string]func(*warpapi.TokenCost, float32){
		"input cents":    (*warpapi.TokenCost).SetInputCostInCents,
		"output cents":   (*warpapi.TokenCost).SetOutputCostInCents,
		"read cents":     (*warpapi.TokenCost).SetInputCacheReadCostInCents,
		"write cents":    (*warpapi.TokenCost).SetInputCacheWriteCostInCents,
		"input credits":  (*warpapi.TokenCost).SetInputCostInCredits,
		"output credits": (*warpapi.TokenCost).SetOutputCostInCredits,
		"read credits":   (*warpapi.TokenCost).SetInputCacheReadCostInCredits,
		"write credits":  (*warpapi.TokenCost).SetInputCacheWriteCostInCredits,
	} {
		setters[name] = func(inference *warpapi.InferenceUsage, value float32) {
			cost := warpapi.TokenCost_builder{}.Build()
			set(cost, value)
			inference.SetTokenCost(cost)
		}
	}
	return setters
}

func TestUsageCurrentProtoMetadataRoundTrip(t *testing.T) {
	inference := warpapi.InferenceUsage_builder{
		WebFetchPageCount:     usageTestPtr(uint32(2)),
		WebFetchCostInCents:   usageTestPtr(float32(0.5)),
		WebFetchCostInCredits: usageTestPtr(float32(0.25)),
	}.Build()
	for number, name := range map[protoreflect.FieldNumber]protoreflect.Name{
		6: "web_fetch_page_count", 7: "web_fetch_cost_in_cents", 8: "web_fetch_cost_in_credits",
	} {
		field := inference.ProtoReflect().Descriptor().Fields().ByNumber(number)
		if field == nil || field.Name() != name {
			t.Fatalf("current metadata field %d not recognized as %s", number, name)
		}
	}
	charged := warpapi.ChargedUsage_builder{
		ClaudeLinkedBillingInferenceUsage: map[string]*warpapi.InferenceUsage{"private-linked-model": inference},
	}.Build()
	field := charged.ProtoReflect().Descriptor().Fields().ByNumber(7)
	if field == nil || field.Name() != "claude_linked_billing_inference_usage" || !field.IsMap() {
		t.Fatal("current linked-billing map field 7 not recognized")
	}
	finish := usageTestFinish(charged)
	data, err := proto.Marshal(finish)
	if err != nil {
		t.Fatal(err)
	}
	var decoded warpapi.ResponseEvent_StreamFinished
	if err := proto.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if hasUnknown(decoded.ProtoReflect()) {
		t.Fatal("current metadata roundtrip introduced unknown fields")
	}
	gotInference := decoded.GetRequestCharges().GetUsageByCategory()["private-category"].GetClaudeLinkedBillingInferenceUsage()["private-linked-model"]
	if !gotInference.HasWebFetchPageCount() || gotInference.GetWebFetchPageCount() != 2 ||
		!gotInference.HasWebFetchCostInCents() || gotInference.GetWebFetchCostInCents() != 0.5 ||
		!gotInference.HasWebFetchCostInCredits() || gotInference.GetWebFetchCostInCredits() != 0.25 {
		t.Fatal("current web-fetch values lost in proto roundtrip")
	}
	usage, err := extractUsage(&decoded)
	if err != nil || usage.InferenceCents == nil || *usage.InferenceCents != 0.5 || usage.InferenceCredits == nil || *usage.InferenceCredits != 0.25 {
		t.Fatalf("current metadata costs not aggregated: %+v, %v", usage, err)
	}
	if !usageHasExternalTools(&decoded) {
		t.Fatal("roundtripped current metadata did not report execution")
	}
}

func TestExtractProbeUsageLinkedBillingNotDuplicated(t *testing.T) {
	inference := warpapi.InferenceUsage_builder{
		TokenCount:           warpapi.TokenCount_builder{Input: usageTestPtr(uint64(10)), Output: usageTestPtr(uint64(4))}.Build(),
		TokenCost:            warpapi.TokenCost_builder{InputCostInCents: usageTestPtr(float32(0)), InputCostInCredits: usageTestPtr(float32(0))}.Build(),
		WebSearchCostInCents: usageTestPtr(float32(1)), WebSearchCostInCredits: usageTestPtr(float32(2)),
		WebFetchCostInCents: usageTestPtr(float32(3)), WebFetchCostInCredits: usageTestPtr(float32(4)),
	}.Build()
	charges := warpapi.RequestCharges_builder{UsageByCategory: map[string]*warpapi.ChargedUsage{
		"private-category": warpapi.ChargedUsage_builder{
			ClaudeLinkedBillingInferenceUsage: map[string]*warpapi.InferenceUsage{"private-model": inference},
		}.Build(),
	}}.Build()
	finish := warpapi.ResponseEvent_StreamFinished_builder{
		RequestCharges: charges,
		RequestCost:    warpapi.ResponseEvent_StreamFinished_RequestCost_builder{Exact: usageTestPtr(float32(99))}.Build(),
		TokenUsage: []*warpapi.ResponseEvent_StreamFinished_TokenUsage{
			warpapi.ResponseEvent_StreamFinished_TokenUsage_builder{TotalInput: usageTestPtr(uint32(99)), CostInCents: usageTestPtr(float32(99))}.Build(),
		},
		ConversationUsageMetadata: warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata_builder{TotalCharges: charges}.Build(),
	}.Build()
	got, err := extractUsage(finish)
	if err != nil || !got.Measured || got.Source != "request_charges" ||
		got.InputTokens == nil || *got.InputTokens != 10 || got.OutputTokens == nil || *got.OutputTokens != 4 ||
		got.InferenceCents == nil || *got.InferenceCents != 4 || got.InferenceCredits == nil || *got.InferenceCredits != 6 {
		t.Fatalf("linked usage omitted or duplicated: %+v, %v", got, err)
	}
	encoded, err := json.Marshal(got)
	if err != nil || string(encoded) != `{"measured":true,"source":"request_charges","input_tokens":10,"output_tokens":4,"inference_cents":4,"inference_credits":6}` {
		t.Fatalf("unexpected aggregate disclosure: %s, %v", encoded, err)
	}
	event, err := json.Marshal(got.event())
	if err != nil || string(event) != `{"inputTokens":10,"outputTokens":4,"warpInferenceCents":4,"warpInferenceCredits":6}` {
		t.Fatalf("unexpected event disclosure: %s, %v", event, err)
	}
}

func TestExtractProbeUsageWebFetchExplicitZero(t *testing.T) {
	for name, set := range usageTestInferenceMapSetters() {
		t.Run(name, func(t *testing.T) {
			charged := warpapi.ChargedUsage_builder{}.Build()
			set(charged, map[string]*warpapi.InferenceUsage{"private-model": warpapi.InferenceUsage_builder{
				WebFetchPageCount:   usageTestPtr(uint32(0)),
				WebFetchCostInCents: usageTestPtr(float32(0)), WebFetchCostInCredits: usageTestPtr(float32(math.Copysign(0, -1))),
			}.Build()})
			finish := usageTestFinish(charged)
			got, err := extractUsage(finish)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(got)
			if err != nil || string(encoded) != `{"measured":true,"source":"request_charges","inference_cents":0,"inference_credits":0}` {
				t.Fatalf("zero fetch cost presence lost: %s, %v", encoded, err)
			}
			if usageHasExternalTools(finish) {
				t.Fatal("explicit-zero web metadata treated as execution")
			}
		})
	}
}

func TestUsageHasExternalToolsAllMaps(t *testing.T) {
	for mapName, setMap := range usageTestInferenceMapSetters() {
		for leafName, setLeaf := range map[string]func(*warpapi.InferenceUsage){
			"search count":   func(u *warpapi.InferenceUsage) { u.SetWebSearchCount(1) },
			"fetch count":    func(u *warpapi.InferenceUsage) { u.SetWebFetchPageCount(1) },
			"search cents":   func(u *warpapi.InferenceUsage) { u.SetWebSearchCostInCents(0.5) },
			"search credits": func(u *warpapi.InferenceUsage) { u.SetWebSearchCostInCredits(0.5) },
			"fetch cents":    func(u *warpapi.InferenceUsage) { u.SetWebFetchCostInCents(0.5) },
			"fetch credits":  func(u *warpapi.InferenceUsage) { u.SetWebFetchCostInCredits(0.5) },
		} {
			t.Run(mapName+"/"+leafName, func(t *testing.T) {
				inference := warpapi.InferenceUsage_builder{
					WebSearchCostInCents: usageTestPtr(float32(0)), WebSearchCostInCredits: usageTestPtr(float32(0)),
					WebFetchCostInCents: usageTestPtr(float32(0)), WebFetchCostInCredits: usageTestPtr(float32(0)),
				}.Build()
				setLeaf(inference)
				charged := warpapi.ChargedUsage_builder{}.Build()
				setMap(charged, map[string]*warpapi.InferenceUsage{"private-model": inference, "nil-model": nil})
				finish := usageTestFinish(charged)
				if !usageHasExternalTools(finish) {
					t.Fatal("positive request web-tool evidence was ignored")
				}
				if _, err := extractUsage(finish); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestUsageHasExternalToolsUnknownAndZero(t *testing.T) {
	positive := warpapi.ChargedUsage_builder{DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{
		"private-model": warpapi.InferenceUsage_builder{WebSearchCount: usageTestPtr(uint32(1)), WebFetchPageCount: usageTestPtr(uint32(1))}.Build(),
	}}.Build()
	cases := map[string]*warpapi.ResponseEvent_StreamFinished{
		"nil":          nil,
		"empty":        warpapi.ResponseEvent_StreamFinished_builder{}.Build(),
		"nil charge":   usageTestFinish(nil),
		"empty charge": usageTestFinish(warpapi.ChargedUsage_builder{}.Build()),
		"conversation only": warpapi.ResponseEvent_StreamFinished_builder{ConversationUsageMetadata: warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata_builder{
			TotalCharges: warpapi.RequestCharges_builder{UsageByCategory: map[string]*warpapi.ChargedUsage{"private-category": positive}}.Build(),
		}.Build()}.Build(),
		"token and platform costs only": usageTestFinish(warpapi.ChargedUsage_builder{
			PlatformUsageInCents: usageTestPtr(float32(100)),
			DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{
				"private-model": warpapi.InferenceUsage_builder{TokenCost: warpapi.TokenCost_builder{InputCostInCents: usageTestPtr(float32(100))}.Build()}.Build(),
			},
		}.Build()),
	}
	for mapName, setMap := range usageTestInferenceMapSetters() {
		charged := warpapi.ChargedUsage_builder{}.Build()
		setMap(charged, map[string]*warpapi.InferenceUsage{
			"nil-model": nil,
			"zero-model": warpapi.InferenceUsage_builder{
				WebSearchCount: usageTestPtr(uint32(0)), WebFetchPageCount: usageTestPtr(uint32(0)),
				WebSearchCostInCents: usageTestPtr(float32(0)), WebSearchCostInCredits: usageTestPtr(float32(0)),
				WebFetchCostInCents: usageTestPtr(float32(0)), WebFetchCostInCredits: usageTestPtr(float32(0)),
			}.Build(),
		})
		cases[mapName+" explicit zero"] = usageTestFinish(charged)
	}
	for name, finish := range cases {
		t.Run(name, func(t *testing.T) {
			if usageHasExternalTools(finish) {
				t.Fatal("no positive request web-tool evidence")
			}
		})
	}
}

func TestExtractProbeUsageTokenOverflow(t *testing.T) {
	for mapName, setMap := range usageTestInferenceMapSetters() {
		for leafName, setLeaf := range map[string]func(*warpapi.TokenCount, uint64){
			"input":       (*warpapi.TokenCount).SetInput,
			"output":      (*warpapi.TokenCount).SetOutput,
			"cache read":  (*warpapi.TokenCount).SetInputCacheRead,
			"cache write": (*warpapi.TokenCount).SetInputCacheWrite,
		} {
			t.Run(mapName+"/"+leafName, func(t *testing.T) {
				models := map[string]*warpapi.InferenceUsage{}
				for name, value := range map[string]uint64{"a": ^uint64(0), "b": 1} {
					count := warpapi.TokenCount_builder{}.Build()
					setLeaf(count, value)
					models[name] = warpapi.InferenceUsage_builder{TokenCount: count}.Build()
				}
				charged := warpapi.ChargedUsage_builder{}.Build()
				setMap(charged, models)
				got, err := extractUsage(usageTestFinish(charged))
				if !errors.Is(err, errUsage) || got != (observedUsage{}) {
					t.Fatalf("overflow was not rejected: %+v, %v", got, err)
				}
			})
		}
	}
}
