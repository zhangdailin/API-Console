package main

import (
	"testing"

	"orchids-api/internal/testutil"
)

func TestProbeErrorReason(t *testing.T) {
	testutil.Equal(t, probeErrorReason("qoder daily request count is exceeded: Billing daily count exceeded"), "Billing daily count exceeded")
	testutil.Equal(t, probeErrorReason("plain"), "plain")
}
