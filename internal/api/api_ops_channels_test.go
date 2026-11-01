package api

import (
	"orchids-api/internal/testutil"
	"testing"
)

// TestIsProviderChannel_KeepsInfrastructureOutOfTheMatrix pins the rule the page
// relies on: the "http" catch-all and our own synthetic probes are counted in the
// overview but are not channels an operator can act on, so they must not appear
// as matrix rows or in the channel picker.
func TestIsProviderChannel_KeepsInfrastructureOutOfTheMatrix(t *testing.T) {
	for _, aggregate := range []string{"http", "probe", "HTTP", " probe ", "Probe"} {
		testutil.Falsef(t, IsProviderChannel(aggregate), "%q must not be presented as a provider channel", aggregate)
	}
	for _, channel := range []string{"grok", "cline", "workbuddy", "GROK"} {
		testutil.True(t, IsProviderChannel(channel), "%q must remain a provider channel")
	}
	testutil.False(t, IsProviderChannel(""), "an empty channel is not a provider channel")
}
