package main

import (
	"math"
	"testing"
)

func TestMinimalGenerationRequiresExplicitObservedModelAndPermission(t *testing.T) {
	for _, tt := range []struct {
		live, generate bool
		model          string
		credits        float64
		valid          bool
	}{
		{true, false, "", 0, true}, {true, true, "claude-test", 1, true},
		{true, true, "", 0, false}, {false, true, "model-a", 0, false}, {true, false, "model-a", 0, false},
		{true, true, "auto-open", 0, false}, {true, true, "auto-efficient", 0, false}, {true, true, "auto", 0, false},
		{true, true, "cli-agent-auto", 0, false}, {true, true, "computer-use-agent-auto", 0, false},
		{true, true, "model-a", math.NaN(), false}, {true, true, "model-a", math.Inf(1), false}, {true, true, "model-a", -1, false},
	} {
		if (validateGenerationPolicy(tt.live, tt.generate, tt.model, tt.credits) == nil) != tt.valid {
			t.Fatalf("generation policy mismatch for %q", tt.model)
		}
	}
}

func TestReportedBudgetIsNotPretendedPrechargeEnforcement(t *testing.T) {
	a, b := 0.25, 0.10
	if checkReportedBudget(probeUsage{}, 1) == nil {
		t.Fatal("missing bill accepted as zero")
	}
	if checkReportedBudget(probeUsage{InferenceCredits: &a}, 1) == nil {
		t.Fatal("partial bill accepted")
	}
	usage := probeUsage{InferenceCredits: &a, PlatformCredits: &b}
	if checkReportedBudget(usage, 0.30) == nil {
		t.Fatal("over-budget observation accepted")
	}
	if checkReportedBudget(usage, 0.40) == nil {
		t.Fatal("partial observations falsely verified a complete under-ceiling bill")
	}
}

func TestProbeModesKeepOAuthAndGenerationSeparate(t *testing.T) {
	for _, m := range []struct {
		live, start, oauth bool
		model              string
		valid              bool
	}{
		{false, false, false, "", true}, {true, false, false, "observed-model", true},
		{false, false, true, "", true}, {false, true, false, "", true},
		{true, false, true, "", false}, {true, true, false, "", false}, {false, true, true, "", false},
		{false, false, true, "observed-model", false}, {false, false, false, "observed-model", false},
	} {
		if (validateProbeModes(m.live, m.start, m.oauth, m.model) == nil) != m.valid {
			t.Fatal("probe stage selection mismatch")
		}
	}
}
