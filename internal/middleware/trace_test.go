package middleware

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type hijackableRecorder struct {
	*httptest.ResponseRecorder
	hijackErr error
	hijacked  bool
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, h.hijackErr
}

func TestGenerateTraceID(t *testing.T) {
	t.Run("generates unique IDs", func(t *testing.T) {
		ids := make(map[string]bool)
		for range 1000 {
			id := GenerateTraceID()
			testutil.CheckFalsef(t, ids[id], "duplicate trace ID generated: %s", id)
			ids[id] = true
		}
	})

	t.Run("generates valid hex string", func(t *testing.T) {
		id := GenerateTraceID()
		if len(id) != 32 { // 16 bytes = 32 hex chars
			t.Errorf("trace ID length = %d, want 32", len(id))
		}
		for _, c := range id {
			testutil.CheckFalsef(t, !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')), "invalid character in trace ID: %c", c)
		}
	})
}

func TestTraceMiddleware(t *testing.T) {
	t.Run("generates trace ID when not provided", func(t *testing.T) {
		handler := TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			traceID := GetTraceID(r.Context())
			testutil.CheckNotEqual(t, traceID, "")
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest("GET", "/", nil)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		testutil.CheckNotEqual(t, w.Header().Get(TraceIDHeader), "")
	})

	t.Run("uses provided trace ID", func(t *testing.T) {
		expectedID := "test-trace-id-123"
		handler := TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			traceID := GetTraceID(r.Context())
			testutil.CheckEqual(t, traceID, expectedID)
		}))

		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set(TraceIDHeader, expectedID)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		testutil.CheckEqual(t, w.Header().Get(TraceIDHeader), expectedID)
	})

	t.Run("uses X-Request-ID as fallback", func(t *testing.T) {
		expectedID := "request-id-456"
		handler := TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			traceID := GetTraceID(r.Context())
			testutil.CheckEqual(t, traceID, expectedID)
		}))

		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set(RequestIDHeader, expectedID)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)
	})
}

func TestGetTraceID(t *testing.T) {
	t.Run("returns empty for context without trace ID", func(t *testing.T) {
		ctx := context.Background()
		testutil.CheckEqual(t, GetTraceID(ctx), "")
	})

	t.Run("returns trace ID from context", func(t *testing.T) {
		expected := "test-trace-id"
		ctx := context.WithValue(context.Background(), traceIDKey{}, expected)
		testutil.CheckEqual(t, GetTraceID(ctx), expected)
	})
}

func TestTracedResponseWriter(t *testing.T) {
	t.Run("tracks status code", func(t *testing.T) {
		w := httptest.NewRecorder()
		traced := NewTracedResponseWriter(w)

		traced.WriteHeader(http.StatusNotFound)

		testutil.CheckEqual(t, traced.StatusCode, http.StatusNotFound)
	})

	t.Run("default status is 200", func(t *testing.T) {
		w := httptest.NewRecorder()
		traced := NewTracedResponseWriter(w)

		testutil.CheckEqual(t, traced.StatusCode, http.StatusOK)
	})

	t.Run("tracks bytes written", func(t *testing.T) {
		w := httptest.NewRecorder()
		traced := NewTracedResponseWriter(w)

		traced.Write([]byte("hello"))
		traced.Write([]byte(" world"))

		testutil.CheckEqual(t, traced.BytesWritten, 11)
	})

	t.Run("flush works", func(t *testing.T) {
		w := httptest.NewRecorder()
		traced := NewTracedResponseWriter(w)

		// Should not panic
		traced.Flush()
	})

	t.Run("flush records an implicit response start", func(t *testing.T) {
		w := httptest.NewRecorder()
		traced := NewTracedResponseWriter(w)
		before := time.Now()
		traced.Flush()
		testutil.Falsef(t, traced.FirstWriteAt().Before(before), "FirstWriteAt=%v, want a flush timestamp", traced.FirstWriteAt())
		testutil.False(t, !w.Flushed, "Flush was not forwarded")
	})

	t.Run("unwrap exposes the underlying writer", func(t *testing.T) {
		w := httptest.NewRecorder()
		traced := NewTracedResponseWriter(w)
		testutil.EqualAny(t, traced.Unwrap(), w)
	})

	t.Run("hijack delegates to underlying writer", func(t *testing.T) {
		w := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
		traced := NewTracedResponseWriter(w)

		_, _, err := traced.Hijack()
		testutil.NoError(t, err, "Hijack() error = %v, want nil")
		testutil.False(t, !w.hijacked, "Hijack() should delegate to underlying writer")
	})

	t.Run("hijack fails when underlying writer does not support it", func(t *testing.T) {
		w := httptest.NewRecorder()
		traced := NewTracedResponseWriter(w)

		_, _, err := traced.Hijack()
		testutil.False(t, err == nil, "Hijack() should fail when underlying writer is not hijackable")
		testutil.MustContain(t, err.Error(), "does not support hijacking")
	})
}

func TestLoggingMiddleware(t *testing.T) {
	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))

	// Add trace middleware first
	handler = TraceMiddleware(handler)

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	testutil.CheckEqual(t, w.Code, http.StatusOK)
}

func TestLoggingMiddleware_WebSocketUpgrade(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	handler := TraceMiddleware(LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte("ok"))
	})))

	server := httptest.NewServer(handler)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	testutil.NoError(t, err, "Dial() error = %v")
	defer conn.Close()

	_, msg, err := conn.ReadMessage()
	testutil.NoError(t, err, "ReadMessage() error = %v")
	testutil.Equal(t, string(msg), "ok")
}

func TestChain(t *testing.T) {
	var order []string

	m1 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "m1-before")
			next.ServeHTTP(w, r)
			order = append(order, "m1-after")
		})
	}

	m2 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "m2-before")
			next.ServeHTTP(w, r)
			order = append(order, "m2-after")
		})
	}

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	})

	chained := Chain(m1, m2)(final)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	chained.ServeHTTP(w, req)

	expected := []string{"m1-before", "m2-before", "handler", "m2-after", "m1-after"}
	testutil.Equal(t, len(order), len(expected))
	for i, v := range expected {
		testutil.CheckEqual(t, order[i], v)
	}
}
