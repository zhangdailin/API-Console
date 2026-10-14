package grok

import (
	"bytes"
	"strings"
	"testing"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestAnthropicStreamMessageStartCarriesEstimatedInput(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"
	var out bytes.Buffer
	testutil.NoError(t, translateOpenAIChatStreamToAnthropicWithInput(&out, strings.NewReader(stream), "grok-4.6", 7))
	first, _, _ := strings.Cut(out.String(), "\n\n")
	testutil.MustContainAll(t, first, `"input_tokens":7`, `"cache_read_input_tokens":0`)
}

func TestApplyCLIBillingDerivesPercentFromMonthly(t *testing.T) {
	info := &CLIBillingInfo{MonthlyLimit: 200, MonthlyUsed: 50, HasMonthly: true}
	acc := &store.Account{}
	ApplyCLIBillingInfo(acc, info)
	testutil.Falsef(t, !acc.GrokBilling.Weekly.HasUsage || acc.GrokBilling.Weekly.UsagePercent != 25, "derived percent not persisted: %+v", acc.GrokBilling)
}
