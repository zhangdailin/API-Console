package ratelimit

import (
	"net/http"
	"orchids-api/internal/testutil"
	"testing"
	"time"
)

func TestParseRateLimitMetadataBuildTeamRPS(t *testing.T) {
	body := []byte(`{"code":"resource-exhausted","error":"Too many requests for team f1692451-874f-4765-ab9b-5285f6c6ff65 and model grok-4.5-build-free. Your team's rate limit is — Requests per Second (actual/limit): 2/2."}`)
	metadata := ParseMetadata(body)
	testutil.False(t, metadata == nil, "expected metadata")
	testutil.Equal(t, metadata.Scope, ScopeRPS)
	testutil.Equal(t, metadata.TeamID, "f1692451-874f-4765-ab9b-5285f6c6ff65")
	testutil.Equal(t, metadata.Model, "grok-4.5-build-free")
	testutil.Equal(t, metadata.Actual, 2)
	testutil.Equal(t, metadata.Limit, 2)
	testutil.Equal(t, metadata.RetryAfter, 2*time.Second)
}

func TestParseRateLimitMetadataRPMWithResetsIn(t *testing.T) {
	body := []byte(`{"code":"resource-exhausted","error":"Too many requests for team 00000000-0000-0000-0000-000000000013 and model grok-4.5. Your team's rate limit is — Requests per Minute (actual/limit): 58/60. Resets in: 45s."}`)
	metadata := ParseMetadata(body)
	testutil.False(t, metadata == nil, "expected metadata")
	testutil.Equal(t, metadata.Scope, ScopeRPM)
	testutil.Equal(t, metadata.TeamID, "00000000-0000-0000-0000-000000000013")
	testutil.Equal(t, metadata.Model, "grok-4.5")
	testutil.Equal(t, metadata.Actual, 58)
	testutil.Equal(t, metadata.Limit, 60)
	// resets-in 45s overrides the default 1m RPM fallback.
	testutil.Equal(t, metadata.RetryAfter, 45*time.Second)
}

func TestParseRateLimitMetadataResetsInHour(t *testing.T) {
	body := []byte(`Too many requests for team f1692451-874f-4765-ab9b-5285f6c6ff65 and model grok-4.5. Requests per Minute (actual/limit): 60/60. Resets in: 1h 30m.`)
	metadata := ParseMetadata(body)
	testutil.False(t, metadata == nil, "expected metadata")
	testutil.Equal(t, metadata.Scope, ScopeRPM)
	testutil.Equal(t, metadata.RetryAfter, 90*time.Minute)
}

func TestParseRateLimitMetadataOrdinary429(t *testing.T) {
	metadata := ParseMetadata([]byte(`{"error":"You are sending requests too quickly"}`))
	testutil.Falsef(t, metadata != nil, "ordinary 429 must not parse as team rate limit: %#v", metadata)
	metadata = ParseMetadata(nil)
	testutil.Falsef(t, metadata != nil, "nil body must not parse: %#v", metadata)
	metadata = ParseMetadata([]byte(`not json at all`))
	testutil.Falsef(t, metadata != nil, "plain text without pattern must not parse: %#v", metadata)
}

func TestRateLimitFromResponseRetryAfterHeader(t *testing.T) {
	body := []byte(`Requests per Second (actual/limit): 6/6 for team 00000000-0000-0000-0000-000000000013 and model grok-4.5`)
	header := http.Header{}
	header.Set("Retry-After", "17")
	metadata := FromResponse(http.StatusTooManyRequests, header, body)
	testutil.False(t, metadata == nil, "expected metadata")
	testutil.Equal(t, metadata.RetryAfter, 17*time.Second)
	testutil.Equal(t, metadata.Scope, ScopeRPS)
}

func TestRateLimitFromResponseNon429(t *testing.T) {
	metadata := FromResponse(http.StatusForbidden, nil, []byte(`Requests per Second (actual/limit): 6/6`))
	testutil.Falsef(t, metadata != nil, "403 must not parse: %#v", metadata)
}
