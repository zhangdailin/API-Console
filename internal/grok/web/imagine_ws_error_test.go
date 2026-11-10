package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"orchids-api/internal/testutil"
)

// imagineErrorSocket answers the first request with the error frame grok.com
// sent on 2026-11-10 for an account whose image allowance was spent.
func imagineErrorSocket(t *testing.T, frame string) *websocket.Conn {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(frame))
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestImagineRateLimitFrameIsATypedUpstreamFailure(t *testing.T) {
	conn := imagineErrorSocket(t, `{"type":"error","err_code":"rate_limit_exceeded","err_msg":"Image rate limit exceeded"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events := make(chan imagineWSEvent, 8)
	finals, err := runImagineWSRound(ctx, conn, "a red apple", "16:9", false, false, 1, events)
	testutil.Equal(t, finals, 0)
	testutil.Error(t, err)
	testutil.True(t, isImagineRateLimit(err), "the rate-limit frame must trigger an account switch")
	testutil.True(t, isUpstreamFailure(err), "an Imagine error frame is an upstream failure, not a caller error")

	// It used to answer 400 invalid_request; it is a rate limit.
	rec := httptest.NewRecorder()
	writeGrokUpstreamError(rec, err)
	testutil.Equal(t, rec.Code, http.StatusTooManyRequests)
	testutil.MustNotContain(t, rec.Body.String(), "invalid_request")
}

func TestImagineOtherErrorFramesDoNotSwitch(t *testing.T) {
	conn := imagineErrorSocket(t, `{"type":"error","err_code":"content_moderated","err_msg":"blocked"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := runImagineWSRound(ctx, conn, "x", "1:1", false, false, 1, make(chan imagineWSEvent, 8))
	testutil.Error(t, err)
	testutil.False(t, isImagineRateLimit(err))
	testutil.True(t, isUpstreamFailure(err))
}
