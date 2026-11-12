package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/testutil"
)

type ssoAuthRoundTripper func(*http.Request) (*http.Response, error)

func (f ssoAuthRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func ssoAuthResponse(status int, location, body string) *http.Response {
	header := http.Header{}
	if location != "" {
		header.Set("Location", location)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func testConsentToken(typ string, claims string) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"ES256","typ":"`+typ+`"}`) + "." + enc(claims) + ".sig"
}

func newTestSSOFlow(rt ssoAuthRoundTripper) *ssoBuildFlow {
	return &ssoBuildFlow{
		client:    &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		userAgent: "test-agent", cookies: map[string]string{"sso": "cookie-value", "sso-rw": "cookie-value"},
	}
}

func TestApproveDeviceCodeWalksVerifyConsentApprove(t *testing.T) {
	token := testConsentToken("consent+jwt", `{"dc":"device-1","aud":"consent"}`)
	var steps []string
	var approveForm url.Values
	flow := newTestSSOFlow(func(r *http.Request) (*http.Response, error) {
		testutil.MustContain(t, r.Header.Get("Cookie"), "sso=cookie-value")
		steps = append(steps, r.Method+" "+r.URL.Host+r.URL.Path)
		switch r.URL.Path {
		case "/oauth2/device/verify":
			return ssoAuthResponse(http.StatusFound, "https://accounts.x.ai/oauth2/device/consent?user_code=ABCD", ""), nil
		case "/oauth2/device/consent":
			return ssoAuthResponse(http.StatusOK, "", `<form><input type="hidden" name="user_code" value="ABCD"><input type="hidden" name="consent_token" value="`+token+`"></form>`), nil
		case "/oauth2/device/approve":
			body, _ := io.ReadAll(r.Body)
			approveForm, _ = url.ParseQuery(string(body))
			testutil.Equal(t, r.Header.Get("Origin"), "https://accounts.x.ai")
			return ssoAuthResponse(http.StatusSeeOther, "/oauth2/device/done", ""), nil
		}
		return nil, fmt.Errorf("unexpected request %s", r.URL)
	})
	testutil.NoError(t, flow.approve(context.Background(), "ABCD", "device-1"))
	testutil.Equal(t, strings.Join(steps, ","), "POST auth.x.ai/oauth2/device/verify,GET accounts.x.ai/oauth2/device/consent,POST auth.x.ai/oauth2/device/approve")
	testutil.Equal(t, approveForm.Get("consent_token"), token)
	testutil.Equal(t, approveForm.Get("action"), "allow")
	testutil.Equal(t, approveForm.Get("user_code"), "ABCD")
}

func TestApproveDeviceCodeClassifiesRefusals(t *testing.T) {
	// The session lands on the sign-in page: the cookie is dead.
	flow := newTestSSOFlow(func(r *http.Request) (*http.Response, error) {
		return ssoAuthResponse(http.StatusFound, "https://accounts.x.ai/sign-in?redirect=x", ""), nil
	})
	testutil.True(t, errors.Is(flow.approve(context.Background(), "ABCD", "device-1"), ErrSSOSessionRejected))

	// Approve is refused for want of a browser check: not a credential verdict.
	token := testConsentToken("consent+jwt", `{"dc":"device-1","aud":"consent"}`)
	flow = newTestSSOFlow(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/oauth2/device/verify":
			return ssoAuthResponse(http.StatusFound, "https://accounts.x.ai/oauth2/device/consent", ""), nil
		case "/oauth2/device/consent":
			return ssoAuthResponse(http.StatusOK, "", `<input name="consent_token" value="`+token+`">`), nil
		}
		return ssoAuthResponse(http.StatusForbidden, "", "Request could not be verified"), nil
	})
	testutil.True(t, errors.Is(flow.approve(context.Background(), "ABCD", "device-1"), ErrBrowserVerificationRequired))

	// A redirect off x.ai must not receive the cookie.
	visited := 0
	flow = newTestSSOFlow(func(r *http.Request) (*http.Response, error) {
		visited++
		return ssoAuthResponse(http.StatusFound, "https://evil.example/oauth2/device/consent", ""), nil
	})
	err := flow.approve(context.Background(), "ABCD", "device-1")
	testutil.True(t, err != nil)
	testutil.Equal(t, visited, 1)

	// A consent page whose token belongs to another authorization is refused.
	other := testConsentToken("consent+jwt", `{"dc":"someone-else","aud":"other"}`)
	flow = newTestSSOFlow(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oauth2/device/verify" {
			return ssoAuthResponse(http.StatusFound, "https://accounts.x.ai/oauth2/device/consent", ""), nil
		}
		testutil.Equal(t, r.URL.Path, "/oauth2/device/consent")
		return ssoAuthResponse(http.StatusOK, "", `<input name="consent_token" value="`+other+`"><input name="user_code" value="ABCD">`), nil
	})
	testutil.True(t, flow.approve(context.Background(), "ABCD", "device-1") != nil)
}

func TestConsentTokenMatchesUserCodeNeedsTheFormEchoAndAnUnexpiredConsentToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	valid := testConsentToken("consent+jwt", `{"dc":"8d0c7c5e-authz","aud":"consent","exp":1800000600}`)
	testutil.True(t, consentTokenMatchesUserCode(valid, "abcd", "ABCD", now))
	testutil.False(t, consentTokenMatchesUserCode(valid, "WXYZ", "ABCD", now), "the form must echo our user code")
	testutil.False(t, consentTokenMatchesUserCode(valid, "ABCD", "ABCD", now.Add(time.Hour)), "expired")
	testutil.False(t, consentTokenMatchesUserCode(testConsentToken("JWT", `{"dc":"x","aud":"consent","exp":1800000600}`), "ABCD", "ABCD", now))
	testutil.False(t, consentTokenMatchesDevice(valid, "device-1"))
	testutil.Equal(t, extractConsentToken([]byte(`{\"consentToken\":\"`+valid+`\"}`)), valid)
}

func TestSafeXAIURL(t *testing.T) {
	testutil.True(t, safeXAIURL("https://auth.x.ai/oauth2/device/verify"))
	testutil.False(t, safeXAIURL("http://auth.x.ai/"))
	testutil.False(t, safeXAIURL("https://x.ai.evil.example/"))
	testutil.False(t, safeXAIURL("https://user@auth.x.ai/"))
}
