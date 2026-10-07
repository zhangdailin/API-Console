package grok

import (
	"errors"
	"fmt"
	"net/http"
	"orchids-api/internal/testutil"
	"testing"
)

func TestIsDefinitiveAccountBlockBody(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"error":{"code":"blocked-user","message":"user is blocked"}}`, true},
		{`{"code":"blocked-user"}`, true},
		{`{"error":"user is blocked"}`, true},
		{`user is blocked`, true},
		{`{"error":{"code":"content_policy_violation"}}`, false},
		{`{"error":"rate limit"}`, false},
		{`{"code":"resource-exhausted"}`, false},
	}
	for _, c := range cases {
		testutil.CheckEqual(t, IsDefinitiveAccountBlockBody([]byte(c.body)), c.want)
	}
}

func TestClassifyUpstreamResponse(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header map[string]string
		body   string
		want   UpstreamErrorKind
	}{
		{name: "429", status: 429, body: "too many requests", want: UpstreamErrorRateLimited},
		{name: "blocked-user", status: 403, body: `{"code":"blocked-user"}`, want: UpstreamErrorAccountBlock},
		{name: "plain 403", status: 403, body: "forbidden", want: UpstreamErrorGenericForbidden},
		{name: "plain 200", status: 200, body: "", want: UpstreamErrorUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			header := make(map[string][]string, len(c.header))
			for k, v := range c.header {
				header[k] = []string{v}
			}
			testutil.Equal(t, ClassifyUpstreamResponse(c.status, http.Header(header), []byte(c.body)), c.want)
		})
	}
}

func TestClassifyUpstreamError_Typed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want UpstreamErrorKind
	}{
		{name: "plain 403", err: &grokUpstreamError{status: 403, body: "forbidden"}, want: UpstreamErrorGenericForbidden},
		{name: "blocked-user", err: &grokUpstreamError{status: 403, body: `{"code":"blocked-user"}`}, want: UpstreamErrorAccountBlock},
		{name: "nil", err: nil, want: UpstreamErrorUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { testutil.Equal(t, ClassifyUpstreamError(c.err), c.want) })
	}
}

func TestClassifyUpstreamError_Wrapped(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want UpstreamErrorKind
	}{
		{name: "wrapped 403", err: fmt.Errorf("attempt failed: %w", newCLIUpstreamError(403, nil, []byte("forbidden"))), want: UpstreamErrorGenericForbidden},
		{name: "wrapped blocked", err: fmt.Errorf("attempt failed: %w", newCLIUpstreamError(403, nil, []byte(`{"code":"blocked-user"}`))), want: UpstreamErrorAccountBlock},
		{name: "wrapped 429", err: fmt.Errorf("attempt failed: %w", newCLIUpstreamError(429, nil, []byte("rate limit exceeded"))), want: UpstreamErrorRateLimited},
		{name: "plain text is not HTTP evidence", err: errors.New("grok upstream status=403 body=blocked-user"), want: UpstreamErrorUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { testutil.Equal(t, ClassifyUpstreamError(c.err), c.want) })
	}
}

func TestUpstreamStatusUsesTypedEvidence(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{nil, 0},
		{newCLIUpstreamError(401, nil, nil), 401},
		{&cliOAuthError{status: 403, message: "invalid_grant"}, 403},
		{fmt.Errorf("attempt: %w", newCLIUpstreamError(429, nil, nil)), 429},
		{errors.New("job status=404 body=missing"), 0},
		{errors.New("grok upstream status=429 body=slow down"), 0},
	} {
		testutil.Equal(t, upstreamStatus(tc.err), tc.want)
	}
}
