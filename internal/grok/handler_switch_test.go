package grok

import (
	"errors"
	"orchids-api/internal/testutil"
	"testing"
	"time"
)

func TestShouldSwitchGrokAccount(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "generic 403", err: newCLIUpstreamError(403, nil, []byte("forbidden")), want: false},
		{name: "blocked-user 403", err: newCLIUpstreamError(403, nil, []byte("{\"code\":\"blocked-user\"}")), want: true},
		{name: "account 429", err: newCLIUpstreamError(429, nil, []byte("rate limit exceeded")), want: true},
		{name: "401", err: newCLIUpstreamError(401, nil, []byte("unauthorized")), want: true},
		{name: "shared 429", err: newCLIUpstreamError(429, nil, []byte("too many requests")), want: true},
		{name: "timeout", err: errors.New("Client.Timeout exceeded while awaiting headers"), want: true},
		{name: "deadline", err: errors.New("context deadline exceeded"), want: true},
		{name: "connection reset", err: errors.New("read: connection reset by peer"), want: true},
		{name: "client canceled", err: errors.New("context canceled"), want: false},
		{name: "other", err: newCLIUpstreamError(404, nil, []byte("model not found")), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { testutil.Equal(t, shouldSwitchGrokAccount(tt.err), tt.want) })
	}
}

func TestUpstreamHTTPResponseStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "429", err: newCLIUpstreamError(429, nil, []byte("too many requests")), want: 429},
		{name: "403", err: newCLIUpstreamError(403, nil, []byte("forbidden")), want: 403},
		{name: "timeout", err: errors.New("context deadline exceeded"), want: 502},
		{name: "none", err: errors.New("grok upstream request failed"), want: 502},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { testutil.Equal(t, upstreamHTTPResponseStatus(tt.err), tt.want) })
	}
}

func TestMarkAllGrokAccountStatuses(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantMark   bool
		wantSwitch bool
	}{
		{name: "plain 403", err: newCLIUpstreamError(403, nil, []byte("forbidden")), wantMark: false, wantSwitch: false},
		{name: "blocked-user 403", err: newCLIUpstreamError(403, nil, []byte("{\"code\":\"blocked-user\"}")), wantMark: true, wantSwitch: true},
		{name: "401", err: newCLIUpstreamError(401, nil, []byte("unauthorized")), wantMark: true, wantSwitch: true},
		{name: "shared synthetic cooldown", err: newSyntheticCooldownError("build:team:abc", "grok-4", 30*time.Second), wantMark: false, wantSwitch: true},
		{name: "structured team 429", err: newCLIUpstreamError(429, nil, []byte("Requests per Minute (actual / limit): 31 / 30 for team 123e4567-e89b-12d3-a456-426614174000 model grok-4.20")), wantMark: false, wantSwitch: true},
		{name: "Build free quota exhausted", err: newCLIUpstreamError(429, nil, []byte("{\"code\":\"resource-exhausted\",\"error\":\"Free usage quota exceeded. Purchase credits\"}")), wantMark: true, wantSwitch: true},
		{name: "plain too many requests", err: newCLIUpstreamError(429, nil, []byte("too many requests")), wantMark: true, wantSwitch: true},
		{name: "account 429", err: newCLIUpstreamError(429, nil, []byte("rate limit exceeded")), wantMark: true, wantSwitch: true},
		{name: "network", err: errors.New("read: connection reset by peer"), wantMark: true, wantSwitch: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testutil.Equal(t, markAllGrokAccountStatuses(tt.err), tt.wantMark)
			testutil.Equal(t, shouldSwitchGrokAccount(tt.err), tt.wantSwitch)
		})
	}
}
