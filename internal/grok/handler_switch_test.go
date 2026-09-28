package grok

import (
	"errors"
	"testing"
)

func TestShouldSwitchGrokAccount(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "generic 403", err: errors.New("grok upstream status=403 body=forbidden"), want: false},
		{name: "blocked-user 403", err: errors.New("grok upstream status=403 body={\"code\":\"blocked-user\"}"), want: true},
		{name: "account 429", err: errors.New("grok upstream status=429 body=rate limit exceeded"), want: true},
		{name: "401", err: errors.New("grok upstream status=401 body=unauthorized"), want: true},
		{name: "shared 429", err: errors.New("grok upstream status=429 body=too many requests"), want: true},
		{name: "timeout", err: errors.New("Client.Timeout exceeded while awaiting headers"), want: true},
		{name: "deadline", err: errors.New("context deadline exceeded"), want: true},
		{name: "connection reset", err: errors.New("read: connection reset by peer"), want: true},
		{name: "client canceled", err: errors.New("context canceled"), want: false},
		{name: "other", err: errors.New("grok upstream status=404 body=model not found"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSwitchGrokAccount(tt.err); got != tt.want {
				t.Fatalf("shouldSwitchGrokAccount(%v)=%v want=%v", tt.err, got, tt.want)
			}
		})
	}
}

func TestUpstreamHTTPResponseStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "429", err: errors.New("grok upstream status=429 body=too many requests"), want: 429},
		{name: "403", err: errors.New("grok upstream status=403 body=forbidden"), want: 403},
		{name: "timeout", err: errors.New("context deadline exceeded"), want: 502},
		{name: "none", err: errors.New("grok upstream request failed"), want: 502},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := upstreamHTTPResponseStatus(tt.err); got != tt.want {
				t.Fatalf("upstreamHTTPResponseStatus(%v)=%v want=%v", tt.err, got, tt.want)
			}
		})
	}
}

func TestMarkAllGrokAccountStatuses(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantMark   bool
		wantSwitch bool
	}{
		{name: "plain 403", err: errors.New("grok upstream status=403 body=forbidden"), wantMark: false, wantSwitch: false},
		{name: "blocked-user 403", err: errors.New("grok upstream status=403 body={\"code\":\"blocked-user\"}"), wantMark: true, wantSwitch: true},
		{name: "401", err: errors.New("grok upstream status=401 body=unauthorized"), wantMark: true, wantSwitch: true},
		{name: "shared synthetic cooldown", err: errors.New("grok upstream status=429 body=too_many_requests team build:team:abc model grok-4 cooling down; retry-after=30s"), wantMark: false, wantSwitch: true},
		{name: "structured team 429", err: errors.New("grok upstream status=429 body=Requests per Minute (actual / limit): 31 / 30 for team 123e4567-e89b-12d3-a456-426614174000 model grok-4.20"), wantMark: false, wantSwitch: true},
		{name: "Build free quota exhausted", err: errors.New("grok upstream status=429 body={\"code\":\"resource-exhausted\",\"error\":\"Free usage quota exceeded. Purchase credits\"}"), wantMark: true, wantSwitch: true},
		{name: "plain too many requests", err: errors.New("grok upstream status=429 body=too many requests"), wantMark: true, wantSwitch: true},
		{name: "account 429", err: errors.New("grok upstream status=429 body=rate limit exceeded"), wantMark: true, wantSwitch: true},
		{name: "network", err: errors.New("read: connection reset by peer"), wantMark: true, wantSwitch: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := markAllGrokAccountStatuses(tt.err); got != tt.wantMark {
				t.Fatalf("markAllGrokAccountStatuses()=%v want mark=%v", got, tt.wantMark)
			}
			if got := shouldSwitchGrokAccount(tt.err); got != tt.wantSwitch {
				t.Fatalf("shouldSwitchGrokAccount()=%v want %v", got, tt.wantSwitch)
			}
		})
	}
}
