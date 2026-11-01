package errors

import (
	"testing"

	"orchids-api/internal/testutil"
)

func TestContentRefusalIsTerminal(t *testing.T) {
	for _, text := range []string{
		`upstream status=403: {"code":"content_policy_violation"}`,
		`upstream status=403: I'm sorry, I can’t help with that request.`,
		`upstream status=403: I cannot help with that request.`,
	} {
		class := ClassifyUpstreamError(text)
		testutil.Equal(t, class.Category, "client")
		testutil.False(t, class.Retryable)
		testutil.False(t, class.SwitchAccount)
	}
	testutil.False(t, IsContentPolicyRefusal("permission-denied: feature requires a paid plan"))
}

func TestUpgradeRequiredIsConfigurationFailure(t *testing.T) {
	class := ClassifyUpstreamError("grok cli upstream status=426 body=upgrade required")
	testutil.Equal(t, class.Category, "upstream_upgrade_required")
	testutil.False(t, class.Retryable)
	testutil.False(t, class.SwitchAccount)
	testutil.Equal(t, StatusForCategory(class.Category), 503)
	testutil.MustContain(t, PublicMessage("upstream status=426"), "newer Build client version")
}
