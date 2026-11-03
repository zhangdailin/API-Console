package web

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	"orchids-api/internal/testutil"
)

func TestWebTierFromProductionQuotaShapes(t *testing.T) {
	windows := func(auto, fast, window int64) map[string]*RateLimitInfo {
		return map[string]*RateLimitInfo{
			"auto": {Limit: auto, HasLimit: true, Remaining: auto, HasRemaining: true, WindowSeconds: window},
			"fast": {Limit: fast, HasLimit: true, Remaining: fast, HasRemaining: true, WindowSeconds: window},
		}
	}
	testutil.Equal(t, inferSubscriptionFromWebQuota(windows(7, 30, 86400)), "basic")
	testutil.Equal(t, inferSubscriptionFromWebQuota(windows(25, 70, 7200)), "lite")
	testutil.Equal(t, inferSubscriptionFromWebQuota(windows(50, 140, 7200)), "super")
	// An unknown shape still says paid or Free through its window length.
	testutil.Equal(t, inferSubscriptionFromWebQuota(windows(33, 99, 7200)), "lite")
	testutil.Equal(t, inferSubscriptionFromWebQuota(windows(33, 99, 86400)), "basic")
	testutil.Equal(t, inferSubscriptionFromWebQuota(windows(33, 99, 0)), "")
}

func TestWeeklyCreditsDistinguishesPaidFromFree(t *testing.T) {
	// Captured from production accounts (billing data only, no credentials).
	paid, _ := hex.DecodeString("000000006d0a6b0d0000c84212001a00220b08cea790d70610d0e4f5152a0b08ce9cb5d70610d0e4f5153a070802150000a6423a07080515000070413a070804150000803f3a070806150000803f421c0802120b08cea790d70610d0e4f5151a0b08ce9cb5d70610d0e4f515580162006801800000000f677270632d7374617475733a300d0a")
	free, _ := hex.DecodeString("00000000300a2e12001a0022060880829ad7062a060880f7bed7064212080212060880829ad7061a060880f7bed706580162006801800000000f677270632d7374617475733a300d0a")
	credits, err := parseWeeklyCredits(paid)
	testutil.NoError(t, err)
	testutil.True(t, credits.Paid)
	testutil.Equal(t, credits.UsagePercent, 100.0)
	testutil.False(t, credits.PeriodEnd.IsZero())
	window := weeklyWindow(credits)
	testutil.Equal(t, window.Remaining, 0.0)

	credits, err = parseWeeklyCredits(free)
	testutil.NoError(t, err)
	testutil.False(t, credits.Paid)
	testutil.False(t, weeklyWindow(credits).HasLimit)
}

func TestLiteImagePayloadIsAPlainFastChat(t *testing.T) {
	lite, ok := ResolveModel("grok-imagine-image-lite")
	testutil.True(t, ok)
	payload := New(nil).appChatImagePayload(lite, "a red boat", "", 1)
	raw, _ := json.Marshal(payload)
	text := string(raw)
	// grok.com refuses image-model fields for Free accounts.
	for _, field := range []string{`"modelName"`, `"modelTier"`, `"requestModelDetails"`, "grok-imagine-image-lite"} {
		testutil.CheckNotContain(t, text, field)
	}
	testutil.Equal(t, payload["modeId"], "fast")
	testutil.Equal(t, payload["enableImageGeneration"], true)
	testutil.Equal(t, payload["message"], "Drawing: a red boat")
}
