package httpclient

import (
	"net/http"
	"orchids-api/internal/testutil"
	"testing"
	"time"
)

func TestSharedHTTPClientHeaderBudgetsAreIsolated(t *testing.T) {
	const key = "synthetic-warp-header-budget"
	ordinary := GetSharedHTTPClientWithLimits(key, 2*time.Hour, nil, false, nil)
	stream := GetSharedHTTPClientWithTimeouts(key, 2*time.Hour, 5*time.Minute, nil, false, nil)
	if ordinary == stream || ordinary.Transport == stream.Transport {
		t.Fatal("stream header policy changed an ordinary shared transport")
	}
	if ordinary.Transport.(*http.Transport).ResponseHeaderTimeout != 120*time.Second || stream.Transport.(*http.Transport).ResponseHeaderTimeout != 5*time.Minute {
		t.Fatal("shared header policies were not preserved")
	}
	if got := GetSharedHTTPClientWithTimeouts(key, 2*time.Hour, 5*time.Minute, nil, false, nil); got != stream {
		t.Fatal("stream connection pool was not reused")
	}
}

func TestResponseHeaderTimeoutForClient(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{name: "default", timeout: 0, want: 30 * time.Second},
		{name: "short", timeout: 20 * time.Second, want: 20 * time.Second},
		{name: "one minute", timeout: 60 * time.Second, want: 60 * time.Second},
		{name: "grok default cap", timeout: 600 * time.Second, want: 120 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testutil.Equal(t, responseHeaderTimeoutForClient(tt.timeout), tt.want)
		})
	}
}

func TestSharedHTTPClientCacheKeyIncludesTimeout(t *testing.T) {
	short := sharedHTTPClientCacheKey("direct", 30*time.Second)
	long := sharedHTTPClientCacheKey("direct", 600*time.Second)
	testutil.NotEqual(t, short, long)
}
