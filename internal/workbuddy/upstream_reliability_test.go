package workbuddy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"sync/atomic"
	"testing"
)

func TestUpstreamReliabilityShortLivedTokenIsReused(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, "{\"code\":0,\"data\":{\"accessToken\":\"new-access\",\"refreshToken\":\"rotated-refresh\",\"expiresIn\":3600}}")
	}))
	defer server.Close()
	c := NewFromAccount(&store.Account{AccountType: "workbuddy", WorkBuddyRefreshToken: "old-refresh", WorkBuddyUID: "uid"}, &config.Config{WorkBuddyBaseURL: server.URL})
	for i := 0; i < 3; i++ {
		if _, err := c.ensureAccessToken(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("3 consecutive token acquisitions caused %d upstream refreshes for fresh 1-hour tokens; want reuse after first refresh", calls.Load())
	}
}
