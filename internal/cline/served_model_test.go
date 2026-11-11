package cline

import (
	"encoding/json"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

func TestConsumeStreamRecordsTheServedModel(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"c1","model":"anthropic/claude-sonnet-4.6","choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"id":"c1","model":"anthropic/claude-sonnet-4.6","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n") + "\n"
	result, err := consumeStreamWithToolNames(strings.NewReader(body), false, nil, nil, false)
	testutil.NoError(t, err, "consume: %v")
	testutil.Equal(t, result.Model, "anthropic/claude-sonnet-4.6")
}

func TestConsumeStreamRecordsTheGatewayProvider(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"c1","model":"deepseek-v4.1-flash","choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"id":"c1","model":"deepseek-v4.1-flash","choices":[{"delta":{},"finish_reason":"stop"}],"provider_metadata":{"gateway":{"routing":{"originalModelId":"deepseek-v4.1-flash","resolvedProvider":"deepseek","finalProvider":"alibaba","totalProviderAttemptCount":2,"candidates":["deepseek","alibaba"]}}}}`,
		`data: [DONE]`,
	}, "\n") + "\n"
	result, err := consumeStreamWithToolNames(strings.NewReader(body), false, nil, nil, false)
	testutil.NoError(t, err, "consume: %v")
	testutil.Equal(t, result.Provider, "alibaba")
	testutil.Equal(t, result.ProviderAttempts, 2)
}

func TestGatewayRoutingReadsEveryPlacementAndToleratesOddTypes(t *testing.T) {
	provider, attempts := gatewayRouting(json.RawMessage(`{"gateway":{"routing":{"resolvedProvider":"deepseek"}}}`))
	testutil.Equal(t, provider, "deepseek")
	testutil.Equal(t, attempts, 0)
	provider, _ = gatewayRouting(nil, json.RawMessage(`{"gateway":{"routing":{"finalProvider":"particle"}}}`))
	testutil.Equal(t, provider, "particle")
	// An unexpected type is ignored, not a protocol error.
	provider, attempts = gatewayRouting(json.RawMessage(`{"gateway":{"routing":{"finalProvider":7,"totalProviderAttemptCount":"3"}}}`))
	testutil.Equal(t, provider, "")
	testutil.Equal(t, attempts, 0)
	provider, attempts = gatewayRoutingFromBody([]byte(`{"error":{"message":"upstream failed","provider_metadata":{"gateway":{"routing":{"finalProvider":"alibaba","totalProviderAttemptCount":3}}}}}`))
	testutil.Equal(t, provider, "alibaba")
	testutil.Equal(t, attempts, 3)
	provider, _ = gatewayRoutingFromBody([]byte(`not json`))
	testutil.Equal(t, provider, "")
}

func TestOddRoutingTypesDoNotBreakTheStream(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"c1","choices":[{"delta":{"content":"hi"}}],"provider_metadata":"opaque"}`,
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"provider_metadata":{"gateway":[]}}`,
		`data: [DONE]`,
	}, "\n") + "\n"
	result, err := consumeStreamWithToolNames(strings.NewReader(body), false, nil, nil, false)
	testutil.NoError(t, err, "consume: %v")
	testutil.Equal(t, result.Provider, "")
	testutil.True(t, result.SawMeaningfulEvent, "the text before the odd block was lost")
}
