package accountpolicy

import (
	"errors"
	"testing"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// clineVercelForbidden is the error production recorded when the provider behind
// one private model refused it. One request walked it across every Cline account
// and marked the whole pool 403.
const clineVercelForbidden = `cline stream error: Failed to create stream: inference request failed: failed to generate stream from Vercel: failed to invoke model 'private/nvidia-nemotron-3.5-super-vl-120b-a12b' with streaming: request failed with status 403: {"error":{"message":"Forbidden","type":"AI_APICallError","param":{"error":"Forbidden","statusCode":403,"name":"AI_APICallError","message":"Forbidden","isRetryable":false,"type":"AI_APICallError"}},"providerMetadata":{"gateway":{"routing":{"originalModelId":"private/nvidia-nemotron-3.5-super-vl-120b-a12b","resolvedProvider":"openai-compatible-private","fallbacksAvailable":[]}}}}`

func TestUpstreamModelRefusalKeepsTheAccountAndStops(t *testing.T) {
	acc := &store.Account{ID: 13, AccountType: "cline", Enabled: true}
	v := Classify(acc, errors.New(clineVercelForbidden), "private/nvidia-nemotron-3.5-super-vl-120b-a12b")

	testutil.Equal(t, v.Status, "")
	testutil.Equal(t, v.Scope, ScopeNone)
	testutil.False(t, v.SwitchAccount, "every account gets the provider's answer; switching only spreads it")
	testutil.False(t, v.Retryable, "the provider's refusal does not change on a retry")

	testutil.Equal(t, apperrors.ClassifyAccountStatus(clineVercelForbidden), "")
	class := apperrors.ClassifyUpstreamError(clineVercelForbidden)
	testutil.Equal(t, class.Category, "model_unavailable")
	testutil.False(t, class.SwitchAccount, "upstream class must not switch either")
}

func TestUpstreamModelRefusalLeavesOtherFailuresAlone(t *testing.T) {
	acc := &store.Account{ID: 13, AccountType: "cline", Enabled: true}
	// The channel itself refusing the credential is still the account's 403.
	v := Classify(acc, errors.New("cline API error: status 403: Forbidden"), "m")
	testutil.Equal(t, v.Status, "403")
	// A provider server error inside the same wrapper stays retryable.
	server := `cline stream error: Failed to create stream: inference request failed: failed to generate stream from Vercel: failed to invoke model 'm' with streaming: request failed with status 502: bad gateway`
	testutil.False(t, apperrors.IsUpstreamModelRefusal(server), "5xx is not a refusal")
	testutil.True(t, Classify(acc, errors.New(server), "m").Retryable, "a provider 5xx keeps the retry")
}
