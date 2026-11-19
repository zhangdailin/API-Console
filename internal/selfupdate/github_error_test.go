package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGitHubCheckFailureClassification(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, message, code string
		status              int
		headers             http.Header
		retry               time.Duration
	}{
		{"primary", "API rate limit exceeded", "github_rate_limited", 403, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(now.Add(40 * time.Minute).Unix())}}, 40 * time.Minute},
		{"secondary", "secondary rate limit", "github_rate_limited", 403, http.Header{"Retry-After": {"120"}, "X-Ratelimit-Remaining": {"50"}}, 2 * time.Minute},
		{"retry date", "slow down", "github_rate_limited", 429, http.Header{"Retry-After": {now.Add(3 * time.Minute).Format(http.TimeFormat)}}, 3 * time.Minute},
		{"header only", "denied", "github_rate_limited", 403, http.Header{"X-Ratelimit-Remaining": {"0"}}, time.Minute},
		{"invalid headers", "rate limit", "github_rate_limited", 403, http.Header{"Retry-After": {"999999999999999999999999999"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"-10"}}, time.Minute},
		{"throttled", "denied", "github_rate_limited", 429, nil, time.Minute},
		{"forbidden", "sensitive-upstream-message", "github_forbidden", 403, nil, 0},
		{"unauthorized", "sensitive-upstream-message", "github_unauthorized", 401, nil, 0},
		{"not found", "sensitive-upstream-message", "github_release_not_found", 404, nil, 0},
		{"unavailable", "sensitive-upstream-message", "github_http_error", 503, nil, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := &http.Response{StatusCode: tt.status, Header: tt.headers, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"message":%q}`, tt.message)))}
			got := githubCheckFailure(res, now)
			if got.Code != tt.code || strings.Contains(got.Error(), "sensitive-upstream-message") {
				t.Fatalf("unexpected error: %+v", got)
			}
			if tt.retry == 0 && !got.RetryAt.IsZero() || tt.retry > 0 && !got.RetryAt.Equal(now.Add(tt.retry)) {
				t.Fatalf("unexpected retry deadline: %v", got.RetryAt)
			}
		})
	}
}

func TestGitHubTokenStaysOnMetadataHost(t *testing.T) {
	t.Setenv("ORCHIDS_UPDATE_GITHUB_TOKEN", " synthetic-private-token ")
	g := NewGitHub("zhangdailin/API-Console")
	calls := 0
	g.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Hostname() == "api.github.com" {
			if r.Header.Get("Authorization") != "Bearer synthetic-private-token" {
				t.Fatal("metadata request lacked the configured token")
			}
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://github.com/zhangdailin/API-Console/releases/latest"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		if r.Header.Get("Authorization") != "" {
			t.Fatal("token leaked to a redirect or download")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"tag_name":"v1.0.3"}`)), Request: r}, nil
	})
	if _, err := g.Latest(context.Background()); err != nil || calls != 2 {
		t.Fatalf("metadata check failed: calls=%d err=%v", calls, err)
	}
	var downloaded strings.Builder
	if err := g.Download(context.Background(), Asset{URL: "https://github.com/zhangdailin/API-Console/releases/download/v1.0.3/server"}, &downloaded, 1024); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com"} {
		r, _ := http.NewRequest("GET", "https://"+host+"/asset", nil)
		r.Header.Set("Authorization", "Bearer synthetic-private-token")
		if err := g.Client.CheckRedirect(r, nil); err != nil || r.Header.Get("Authorization") != "" {
			t.Fatalf("unsafe redirect to %s", host)
		}
	}
}

func TestGitHubAnonymousAndNetworkErrorPrivacy(t *testing.T) {
	t.Setenv("ORCHIDS_UPDATE_GITHUB_TOKEN", "")
	g := NewGitHub("zhangdailin/API-Console")
	g.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("anonymous check included authentication")
		}
		return nil, errors.New("proxy URL contains synthetic-secret")
	})
	_, err := g.Latest(context.Background())
	var failure *releaseCheckError
	if !errors.As(err, &failure) || failure.Code != "github_unreachable" || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatalf("network error was not safely classified: %v", err)
	}
}

func TestInvalidGitHubTokenDoesNotFallBackToAnonymous(t *testing.T) {
	t.Setenv("ORCHIDS_UPDATE_GITHUB_TOKEN", "synthetic-private-token")
	g := NewGitHub("zhangdailin/API-Console")
	calls := 0
	g.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"synthetic-private-token"}`)), Request: r}, nil
	})
	_, err := g.Latest(context.Background())
	var failure *releaseCheckError
	if !errors.As(err, &failure) || failure.Code != "github_unauthorized" || strings.Contains(err.Error(), "synthetic-private-token") || calls != 1 {
		t.Fatalf("unsafe authentication failure: calls=%d err=%v", calls, err)
	}
}
