package warp

import (
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestUsageStreamContractPreservesRawComponents(t *testing.T) {
	usage := observedUsage{
		Source: "request_charges", Measured: true,
		InputTokens: usageTestPtr(uint64(130)), CacheReadTokens: usageTestPtr(uint64(26892)),
		CacheWriteTokens: usageTestPtr(uint64(64)), OutputTokens: usageTestPtr(uint64(506)),
	}
	stream := usage.streamUsageEvent()
	if stream["inputTokensSemantics"] != "exclusive_cache" || stream["inputTokens"] != 130 || stream["cacheReadTokens"] != 26892 || stream["cacheWriteTokens"] != 64 || stream["outputTokens"] != 506 {
		t.Fatalf("stream contract changed raw components: %v", stream)
	}
	raw := usage.event()
	if len(raw) != 4 || raw["inputTokens"] != 130 {
		t.Fatalf("diagnostics must remain raw numeric observations: %v", raw)
	}
	if _, exists := raw["inputTokensSemantics"]; exists {
		t.Fatal("semantic marker leaked into numeric diagnostics")
	}
}

func TestUsageStreamContractPreservesUnknownAndExplicitZero(t *testing.T) {
	usage := observedUsage{Source: "request_charges", CacheReadTokens: usageTestPtr(uint64(0))}
	stream := usage.streamUsageEvent()
	if len(stream) != 2 || stream["cacheReadTokens"] != 0 || stream["inputTokensSemantics"] != "exclusive_cache" {
		t.Fatalf("unknown fields became zero: %v", stream)
	}
	for _, field := range []string{"inputTokens", "outputTokens", "cacheWriteTokens"} {
		if _, exists := stream[field]; exists {
			t.Fatalf("invented %s", field)
		}
	}
}

func TestUsageStreamContractDoesNotReinterpretLegacyTotalInput(t *testing.T) {
	usage := observedUsage{Source: "legacy_request_usage", InputTokens: usageTestPtr(uint64(300))}
	stream := usage.streamUsageEvent()
	if len(stream) != 1 || stream["inputTokens"] != 300 {
		t.Fatalf("legacy total_input must remain unmarked: %v", stream)
	}
}

func TestUsageStreamContractOmitsUnrepresentableCount(t *testing.T) {
	usage := observedUsage{Source: "request_charges", InputTokens: usageTestPtr(^uint64(0)), OutputTokens: usageTestPtr(uint64(506))}
	stream := usage.streamUsageEvent()
	if _, present := stream["inputTokens"]; present {
		t.Fatalf("overflowing count was converted to an int: %v", stream)
	}
	if stream["outputTokens"] != 506 {
		t.Fatalf("overflowing input discarded independent output: %v", stream)
	}
}

func TestUsageIncompleteContributorsRemainNonAuthoritative(t *testing.T) {
	finish := usageTestFinish(warpapi.ChargedUsage_builder{DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{
		"complete": warpapi.InferenceUsage_builder{TokenCount: warpapi.TokenCount_builder{Input: proto.Uint64(130), Output: proto.Uint64(506), InputCacheRead: proto.Uint64(26892), InputCacheWrite: proto.Uint64(0)}.Build()}.Build(),
		"partial":  warpapi.InferenceUsage_builder{TokenCount: warpapi.TokenCount_builder{Output: proto.Uint64(1)}.Build()}.Build(),
	}}.Build())
	u, err := extractUsage(finish)
	if err != nil {
		t.Fatal(err)
	}
	if !u.IncompleteContributors || u.streamUsageEvent()["inputTokensIncomplete"] != true || u.event()["outputTokens"] != 507 {
		t.Fatal("partial category lost missingness or observation")
	}
	if _, exists := u.event()["inputTokensIncomplete"]; exists {
		t.Fatal("semantic completeness flag leaked in rawdiagnostic")
	}
}
