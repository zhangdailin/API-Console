package warp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type publicConfigTransport func(*http.Request) (*http.Response, error)

func (f publicConfigTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func clearPublicConfig(t *testing.T) {
	t.Helper()
	publicProjectConfig.Lock()
	old := publicProjectConfig.key
	publicProjectConfig.key = ""
	publicProjectConfig.Unlock()
	t.Cleanup(func() { publicProjectConfig.Lock(); publicProjectConfig.key = old; publicProjectConfig.Unlock() })
}
func fixtureProjectKey() string { return "AIza" + strings.Repeat("x", 35) }
func TestPublicProjectConfigAcceptsOnlyUniqueOfficialShape(t *testing.T) {
	key := fixtureProjectKey()
	valid := `firebase_auth_api_key: "` + key + `".into(),`
	got, err := parsePublicProjectConfig([]byte(valid))
	if err != nil || got != key {
		t.Fatal("valid public shape rejected")
	}
	for _, raw := range []string{"", `firebase_auth_api_key: "wrong".into()`, valid + valid} {
		if _, err := parsePublicProjectConfig([]byte(raw)); !errors.Is(err, errPublicConfig) {
			t.Fatal("ambiguous project source accepted")
		}
	}
}
func TestPublicProjectConfigLoadsOnlyPinnedSourceAndCaches(t *testing.T) {
	clearPublicConfig(t)
	var calls atomic.Int64
	c := NewFromAccount(nil, nil)
	c.http = &http.Client{Transport: publicConfigTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.String() != publicConfigURL || r.Header.Get("Authorization") != "" {
			t.Error("unexpected public config request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`firebase_auth_api_key: "` + fixtureProjectKey() + `".into(),`))}, nil
	})}
	if err := c.ensureProjectConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	other := NewFromAccount(nil, nil)
	other.http = c.http
	if err := other.ensureProjectConfig(context.Background()); err != nil || other.firebaseKey != fixtureProjectKey() || calls.Load() != 1 {
		t.Fatal("official config not cached")
	}
}
func TestPublicProjectConfigDoesNotLeakOrFetchForCustomEndpoints(t *testing.T) {
	clearPublicConfig(t)
	c := NewFromAccount(nil, nil)
	c.http = &http.Client{Transport: publicConfigTransport(func(r *http.Request) (*http.Response, error) { return nil, errors.New("private proxy or key detail") })}
	err := c.ensureProjectConfig(context.Background())
	if err != errPublicConfig || strings.Contains(err.Error(), "private") {
		t.Fatal("unsafe public config failure")
	}
	c.base = "https://custom.invalid"
	calls := 0
	c.http = &http.Client{Transport: publicConfigTransport(func(r *http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected") })}
	if c.ensureProjectConfig(context.Background()) != errPublicConfig || calls != 0 {
		t.Fatal("production key sent to overridden endpoint")
	}
}
