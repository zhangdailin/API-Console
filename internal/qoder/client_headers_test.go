package qoder

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

func TestReferenceRuntimeIdentityRebuiltAfterTokenRotation(t *testing.T) {
	acc := signedTestAccount()
	acc.ID = 0
	client := NewFromAccount(acc, nil)
	initial := client.currentCredentials()
	before, err := client.ensureRuntimeFields(context.Background(), initial)
	testutil.NoError(t, err)
	again, err := client.ensureRuntimeFields(context.Background(), initial)
	testutil.Equal(t, err, nil)
	testutil.Equal(t, again, before)
	rotated := initial
	rotated.AccessToken = "new-access"
	rotated.RefreshToken = "new-refresh"
	client.storeCredentials(rotated, true, initial.RefreshToken)
	after, err := client.ensureRuntimeFields(context.Background(), rotated)
	testutil.NoError(t, err)
	testutil.False(t, after == before || !after.Complete(), "runtime identity was not renewed after the credential rotated")
}

// TestEnsureRuntimeFieldsDerivesOnce proves the pair is derived on demand and
// then reused, which is what keeps a request from re-deriving the identity
// material on every call.
func TestEnsureRuntimeFieldsDerivesOnce(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	acc.QoderRuntimeInfo = ""
	acc.QoderRuntimeKey = ""
	client := NewFromAccount(acc, nil)
	setTestEntropy(client, strings.NewReader(strings.Repeat("\x11", 4096)))

	first, err := client.ensureRuntimeFields(context.Background(), credsOf(acc))
	testutil.NoError(t, err, "ensureRuntimeFields() error = %v")
	testutil.False(t, !first.Complete(), "the derived pair is incomplete")
	second, err := client.ensureRuntimeFields(context.Background(), credsOf(acc))
	testutil.NoError(t, err, "second ensureRuntimeFields() error = %v")
	testutil.Equal(t, first, second)
}

// TestApplyAuthHeadersOmitsOrganizationWhenAbsent pins the conditional presence
// rule: the gateway rejects an empty organization header.
func TestApplyAuthHeadersOmitsOrganizationWhenAbsent(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	creds := credsOf(acc)
	fields := RuntimeFields{EncryptUserInfo: "info", Key: "key"}

	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/algo/api/v2/quota/usage?Encode=1", nil)
	testutil.NoError(t, err)
	testutil.NoError(t, client.applyAuthHeaders(req, creds, fields, "req-1", "", "", "", signPath(req.URL.String())), "applyAuthHeaders() error = %v")
	testutil.CheckEqual(t, req.Header.Get("Cosy-Organization-Id"), "")
	testutil.CheckEqual(t, req.Header.Get("X-Model-Key"), "")

	creds.OrgID = "org-1"
	creds.OrgTags = []string{"a", "b"}
	req2, err := http.NewRequest(http.MethodPost, "https://example.invalid/algo/api/v2/service/pro/sse/agent_chat_generation", nil)
	testutil.NoError(t, err)
	testutil.NoError(t, client.applyAuthHeaders(req2, creds, fields, "req-2", "dmodel", "", "body", signPath(req2.URL.String())), "applyAuthHeaders() error = %v")
	testutil.CheckEqual(t, req2.Header.Get("Cosy-Organization-Id"), "org-1")
	testutil.CheckEqual(t, req2.Header.Get("Cosy-Organization-Tags"), "a,b")
	if got := req2.Header.Get("X-Model-Source"); got != "" {
		// The source header is gated on the key, not on its own value; an empty
		// source must still be present when a key is sent.
		t.Errorf("X-Model-Source = %q, want present and empty", got)
	}
}
