package pricing

import (
	"math"
	"testing"

	"orchids-api/internal/testutil"
)

func TestVoicePrices(t *testing.T) {
	result, ok := EstimateTTSCost("你好 world")
	testutil.True(t, ok)
	// 8 Unicode characters at $15 per 1M characters.
	testutil.Equal(t, result.CostInUSDTicks, int64(8*150_000))
	_, ok = EstimateTTSCost("")
	testutil.False(t, ok)

	result, ok = EstimateSTTCost(3600, false)
	testutil.True(t, ok)
	testutil.Equal(t, result.CostInUSDTicks, int64(1_000_000_000))
	testutil.Equal(t, result.Model, "grok-stt-rest")
	result, _ = EstimateSTTCost(1e-9, true)
	testutil.Equal(t, result.CostInUSDTicks, int64(1))
	_, ok = EstimateSTTCost(math.NaN(), false)
	testutil.False(t, ok)
}

func TestConsoleRoutesArePricedAtTheirUpstreamModel(t *testing.T) {
	plain, ok := EstimateCost("grok-4.5", 1000, 0, 100, 1000)
	testutil.True(t, ok)
	qualified, ok := EstimateCost("console/grok-4.5", 1000, 0, 100, 1000)
	testutil.True(t, ok)
	testutil.Equal(t, qualified.CostInUSDTicks, plain.CostInUSDTicks)
	_, ok = EstimateTextReservationFromBody([]byte(`{"model":"console/grok-4.5","messages":[]}`))
	testutil.True(t, ok, "a Console chat request holds a reservation")
}
