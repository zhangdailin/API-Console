package web

import (
	"errors"
	"net/http"
	"testing"

	"orchids-api/internal/testutil"
)

// webSpentAllowance has the Build plane's production refusal body in the Web
// error shape. No grok.com 402 has been captured; the Web plane switches on it
// defensively, as Build does.
const webSpentAllowance = `grok upstream status=402 body={"error":"You have run out of credits or need a Grok subscription."}`

// TestWebSpentAccountSwitches: one account's spent allowance must not end the
// request while other accounts can serve it.
func TestWebSpentAccountSwitches(t *testing.T) {
	for name, err := range map[string]error{
		"text 402":     errors.New(webSpentAllowance),
		"typed 402":    &grokUpstreamError{status: http.StatusPaymentRequired, body: `{"error":"payment required"}`},
		"credits text": errors.New(`grok upstream body={"error":"no AI credits remaining"}`),
	} {
		testutil.True(t, shouldSwitchGrokAccount(err), name+": a spent account did not switch")
		testutil.True(t, markAllGrokAccountStatuses(err), name+": a spent account was not marked")
	}
}

// TestWebForbiddenStillDoesNotSwitch guards the order of the checks: the new
// branch must not reach a generic 403 or an egress challenge.
func TestWebForbiddenStillDoesNotSwitch(t *testing.T) {
	generic := &grokUpstreamError{status: http.StatusForbidden, body: `{"error":"feature not available"}`}
	testutil.False(t, shouldSwitchGrokAccount(generic), "a generic 403 switched")

	challenge := &grokUpstreamError{status: http.StatusForbidden, header: http.Header{"Cf-Mitigated": {"challenge"}}}
	testutil.False(t, shouldSwitchGrokAccount(challenge), "a Cloudflare challenge switched")
}
