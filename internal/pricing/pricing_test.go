package pricing

import (
	"orchids-api/internal/testutil"
	"strings"
	"testing"
)

// TestEstimateCostUsesOfficialRates pins the transcribed rate table: each entry
// is the published USD-per-1M price multiplied by 1e6 ticks.
func TestEstimateCostUsesOfficialRates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		model     string
		canonical string
		// ticks per single token: uncached input, cached input, output
		input, cached, output int64
	}{
		{"build", "grok-composer-2.5-fast", "grok-composer-2.5-fast", 10000, 2000, 20000},
		{"build 4.6", "grok-4.6", "grok-4.6", 20000, 5000, 60000},
		{"build 4.5", "grok-4.5", "grok-4.5", 20000, 3000, 60000},
		{"current effort", "grok-4.6-high", "grok-4.6", 20000, 5000, 60000},
		{"current xhigh", "grok-4.6-xhigh", "grok-4.6", 20000, 5000, 60000},
		{"current composer effort", "grok-composer-2.5-fast-none", "grok-composer-2.5-fast", 10000, 2000, 20000},
		// Source prefixes are stripped before resolution.
		{"build prefix", "build/grok-4.6", "grok-4.6", 20000, 5000, 60000},
		{"case and space", "  Grok-4.6  ", "grok-4.6", 20000, 5000, 60000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := EstimateCost(tc.model, 1, 0, 0, 0)
			testutil.Falsef(t, !ok || got.Model != tc.canonical || got.CostInUSDTicks != tc.input, "input EstimateCost(%q) = %#v, %v; want %s / %d", tc.model, got, ok, tc.canonical, tc.input)
			got, ok = EstimateCost(tc.model, 1, 1, 0, 0)
			testutil.Falsef(t, !ok || got.CostInUSDTicks != tc.cached, "cached EstimateCost(%q) = %#v, %v; want %d", tc.model, got, ok, tc.cached)
			got, ok = EstimateCost(tc.model, 0, 0, 1, 0)
			testutil.Falsef(t, !ok || got.CostInUSDTicks != tc.output, "output EstimateCost(%q) = %#v, %v; want %d", tc.model, got, ok, tc.output)
		})
	}
}

// TestEstimateCostMatchesPublishedPerMillionPrices checks a whole million of
// each component, which is how the rates are published.
func TestEstimateCostMatchesPublishedPerMillionPrices(t *testing.T) {
	t.Parallel()

	got, ok := EstimateCost("grok-4.6", 100_000, 0, 100_000, 100_000)
	testutil.True(t, ok, "grok-4.6 is priced")
	// 0.1M in at $2 + 0.1M out at $6 = $0.80 = 8e9 ticks (standard tier).
	want := int64(8) * 1_000_000_000
	testutil.Falsef(t, got.CostInUSDTicks != want, "cost = %d ticks, want %d", got.CostInUSDTicks, want)
	got, ok = EstimateCost("grok-composer-2.5-fast", 100_000, 0, 0, 0)
	testutil.Falsef(t, !ok || got.CostInUSDTicks != 1_000_000_000, "build input cost = %#v, %v; want $0.10", got, ok)
}

// TestEstimateCostLongContextSwitch pins the >200k switch: exactly 200k is the
// standard column, one token more is the long-context column.
func TestEstimateCostLongContextSwitch(t *testing.T) {
	t.Parallel()

	atLimit, ok := EstimateCost("grok-4.6", 200_000, 0, 1, 200_000)
	testutil.Falsef(t, !ok || atLimit.CostInUSDTicks != 200_000*20000+60000, "at limit = %#v, %v", atLimit, ok)
	overLimit, ok := EstimateCost("grok-4.6", 200_001, 0, 1, 200_001)
	testutil.Falsef(t, !ok || overLimit.CostInUSDTicks != 200_001*40000+120000, "over limit = %#v, %v", overLimit, ok)
	// A zero context size falls back to the billed input size.
	implicit, ok := EstimateCost("grok-4.6", 200_001, 0, 0, 0)
	testutil.Falsef(t, !ok || implicit.CostInUSDTicks != 200_001*40000, "implicit context = %#v, %v", implicit, ok)
}

func TestEstimateCostClampsInvalidInputs(t *testing.T) {
	t.Parallel()

	// Cached can never exceed input, and negatives never credit the caller.
	got, ok := EstimateCost("grok-4.6", 100, 5_000, 0, 0)
	testutil.Falsef(t, !ok || got.CostInUSDTicks != 100*5000, "cached clamp = %#v, %v", got, ok)
	got, ok = EstimateCost("grok-4.6", -10, -10, -10, -10)
	testutil.Falsef(t, !ok || got.CostInUSDTicks != 0, "negative clamp = %#v, %v", got, ok)
}

func TestEstimateCostUnknownModelIsNotZeroCost(t *testing.T) {
	t.Parallel()

	for _, model := range []string{"", "   ", "grok-imagine-image", "gpt-5", "claude-sonnet-4", "other/grok-4.6", "grok-4.5-latest", "grok-code-fast", "grok-4.3", "grok-4.20", "grok_build/grok-4.6", "grok-4.6-0309-reasoning", "grok-4.5-xhigh", "grok-4.6-none", "grok-4.6-future", "grok-4.3-high"} {
		got, ok := EstimateCost(model, 1_000, 0, 1_000, 0)
		testutil.Falsef(t, ok, "EstimateCost(%q) = %#v, want unpriced", model, got)
	}
}

func TestEstimateTextReservationFromBodyMatchesLegacyEstimator(t *testing.T) {
	bodies := [][]byte{
		[]byte(`{"model":"grok-4.6","max_tokens":1000,"messages":[{"role":"user","content":"hello"}]}`),
		[]byte(`{"model":"grok-4.6-high","max_tokens":1000,"messages":[{"role":"user","content":"hello"}]}`),
		[]byte(`{"messages":[{"content":[{"type":"text","text":"你好"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}],"max_completion_tokens":42,"model":"grok-4.5"}`),
		[]byte(`{"model":"build/grok-4.6","max_output_tokens":7,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`),
	}
	for _, body := range bodies {
		got, ok := EstimateTextReservationFromBody(body)
		testutil.Falsef(t, !ok || got.Model == "" || got.CostInUSDTicks <= 0, "body=%s got=%#v ok=%v", body, got, ok)
	}
	malformed, ok := EstimateTextReservationFromBody([]byte(`{"model":"grok-4.6"`))
	testutil.False(t, !ok || malformed.CostInUSDTicks <= 0, "a truncated body with a complete model must use the conservative fallback")
}

func BenchmarkEstimateTextReservationFromBody(b *testing.B) {
	body := []byte(`{"model":"grok-4.6","max_tokens":4096,"messages":[{"role":"user","content":"` + strings.Repeat("large prompt ", 10000) + `"}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		_, _ = EstimateTextReservationFromBody(body)
	}
}

func TestConstantsAreStable(t *testing.T) {
	t.Parallel()

	testutil.Equal(t, TicksPerUSD, 10_000_000_000)
	testutil.Falsef(t, Version != "official-"+AsOf || Source == "", "version %q / source %q", Version, Source)
}

func TestReconstructBreakdownExplainsAStoredCost(t *testing.T) {
	// A text row: the components must add up to the estimator's answer.
	q := Quantities{InputTokens: 1000, CachedTokens: 400, OutputTokens: 500}
	breakdown, ok := ReconstructBreakdown("grok-4.6", q)
	testutil.True(t, ok, "grok-4.6 was not reconstructed")
	direct, priced := EstimateCost("grok-4.6", q.InputTokens, q.CachedTokens, q.OutputTokens, q.InputTokens)
	testutil.Falsef(t, !priced || breakdown.CostInUSDTicks != direct.CostInUSDTicks, "breakdown=%d direct=%d", breakdown.CostInUSDTicks, direct.CostInUSDTicks)
	kinds := map[ComponentKind]int64{}
	for _, component := range breakdown.Components {
		kinds[component.Kind] = component.Quantity
	}
	testutil.Falsef(t, kinds[ComponentUncachedInput] != 600 || kinds[ComponentCachedInput] != 400 || kinds[ComponentOutput] != 500, "components=%+v", breakdown.Components)
	// The long-context tier is visible in the component price.
	long, ok := ReconstructBreakdown("grok-4.6", Quantities{InputTokens: 300_000, OutputTokens: 10, ContextTokens: 300_000})
	testutil.True(t, ok, "long-context row was not reconstructed")
	for _, component := range long.Components {
		testutil.Falsef(t, component.Kind == ComponentUncachedInput && component.UnitPriceInUSDTicks != 40_000, "long-context input price=%d", component.UnitPriceInUSDTicks)
	}

}
