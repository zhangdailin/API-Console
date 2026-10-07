package grok

import (
	"context"
	"io"
	"net/http"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"strings"
	"testing"
	"time"
)

type auditOAuthBody struct {
	io.Reader
	cancel context.CancelFunc
}

func (b *auditOAuthBody) Close() error {
	if b.cancel != nil {
		b.cancel()
	}
	return nil
}

type auditOAuthTransport struct {
	cancel context.CancelFunc
	calls  int
}

func (tr *auditOAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.calls++
	status := 200
	body := "{\"access_token\":\"rotated-access\",\"refresh_token\":\"rotated-refresh\",\"expires_in\":3600}"
	if tr.calls > 1 {
		status = 400
		body = "{\"error\":\"invalid_grant\"}"
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: &auditOAuthBody{Reader: strings.NewReader(body), cancel: tr.cancel}}, nil
}
func TestUpstreamReliabilityGrokRotationSurvivesRequestCancellation(t *testing.T) {
	s := newTestGrokStore(t, "audit:")
	acc := &store.Account{AccountType: "grok", CredentialType: "oauth", Enabled: true, OAuthAccessToken: "stale", OAuthRefreshToken: "old-refresh", OAuthExpiresAt: time.Now().Add(-time.Hour)}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := &auditOAuthTransport{cancel: cancel}
	o := NewCLIOAuth(&config.Config{GrokCLIOAuthTokenURL: "https://audit.invalid/token"}, &http.Client{Transport: tr})
	o.SetAccountStore(s)
	token, err := o.AccessToken(ctx, acc)
	t.Logf("first refresh returned token-present=%v err=%v", token != "", err)
	got, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, nextErr := o.AccessToken(context.Background(), acc)
	if got.OAuthRefreshToken != "rotated-refresh" {
		t.Errorf("rotation not durable after cancellation; next request error=%v; refresh calls=%d", nextErr, tr.calls)
	}
}

func TestPendingGrokRotationYieldsToReauthorization(t *testing.T) {
	s := newTestGrokStore(t, "grok-pending:")
	acc := &store.Account{AccountType: "grok", Enabled: true, OAuthAccessToken: "reauthorized-access", OAuthRefreshToken: "reauthorized-refresh", OAuthExpiresAt: time.Now().Add(time.Hour)}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	key := cliRotationKey{s, acc.ID}
	cliOAuthPendingRotations.Store(key, store.GrokCredentialPatch{ExpectedRefreshToken: "old", AccessToken: "stale-pending-access", RefreshToken: "old-rotated"})
	defer cliOAuthPendingRotations.Delete(key)
	tr := &auditOAuthTransport{}
	o := NewCLIOAuth(nil, &http.Client{Transport: tr})
	o.SetAccountStore(s)
	token, err := o.AccessToken(context.Background(), acc)
	if err != nil || token != "reauthorized-access" || tr.calls != 0 {
		t.Fatalf("pending rotation blocked new authorization: calls=%d err=%v", tr.calls, err)
	}
}
