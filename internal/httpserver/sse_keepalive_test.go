package httpserver

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/middleware"
	"orchids-api/internal/testutil"
)

func TestSSEKeepalivePreservesUncommittedAndErrorResponses(t *testing.T) {
	for _, status := range []int{0, http.StatusForbidden, http.StatusServiceUnavailable} {
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "text/event-stream")
		w := NewSSEKeepalive(rec, time.Hour, nil)
		if status != 0 {
			w.WriteHeader(status)
		}
		testutil.NoError(t, w.heartbeat(time.Now().Add(2*time.Hour)))
		testutil.NoError(t, w.Close())
		testutil.Equal(t, rec.Body.Len(), 0)
		testutil.False(t, rec.Flushed)
		if status != 0 {
			testutil.Equal(t, rec.Code, status)
		}
	}
	// Non-SSE success responses must never receive comments either.
	rec := httptest.NewRecorder()
	w := NewSSEKeepalive(rec, time.Hour, nil)
	w.WriteHeader(http.StatusOK)
	testutil.NoError(t, w.heartbeat(time.Now().Add(2*time.Hour)))
	testutil.NoError(t, w.Close())
	testutil.Equal(t, rec.Body.Len(), 0)
}

func TestSSEKeepaliveOnlyWritesBetweenCompleteFrames(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "text/event-stream")
		w := NewSSEKeepalive(rec, time.Hour, nil)
		w.WriteHeader(http.StatusOK)
		testutil.NoError(t, w.heartbeat(time.Now()))
		testutil.Equal(t, rec.Body.Len(), 0)
		_, err := w.Write([]byte("event: demo" + newline + `data: {"text":"par`))
		testutil.NoError(t, err)
		testutil.NoError(t, w.heartbeat(time.Now().Add(2*time.Hour)))
		testutil.MustNotContain(t, rec.Body.String(), "keepalive")
		_, err = w.Write([]byte(`tial"}` + newline + newline))
		testutil.NoError(t, err)
		testutil.NoError(t, w.heartbeat(time.Now().Add(2*time.Hour)))
		testutil.Equal(t, rec.Body.String(), "event: demo"+newline+`data: {"text":"partial"}`+newline+newline+": keepalive\n\n")
		testutil.True(t, rec.Flushed)
		testutil.NoError(t, w.Close())
	}
}

func TestSSEKeepaliveJoinsAndUnblocksReaderOnFlushFailure(t *testing.T) {
	failure := errors.New("client disconnected")
	target := &downstreamFailureWriter{ResponseRecorder: httptest.NewRecorder(), flushErr: failure}
	target.Header().Set("Content-Type", "text/event-stream")
	reader, writer := io.Pipe()
	defer writer.Close()
	defer reader.Close()
	w := NewSSEKeepalive(target, time.Millisecond, func() { _ = reader.CloseWithError(failure) })
	w.WriteHeader(http.StatusOK)
	readDone := make(chan error, 1)
	go func() { _, err := reader.Read(make([]byte, 1)); readDone <- err }()
	select {
	case err := <-readDone:
		testutil.True(t, err != nil)
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat failure did not unblock upstream reader")
	}
	testutil.True(t, errors.Is(w.Close(), failure))
	testutil.True(t, errors.Is(w.Close(), failure))
	_, err := w.Write([]byte("data: ignored\n\n"))
	testutil.True(t, errors.Is(err, failure))
}

func TestSSEKeepaliveConcurrentFramesRemainValid(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	w := NewSSEKeepalive(rec, time.Hour, nil)
	w.WriteHeader(http.StatusOK)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			_ = w.heartbeat(time.Now().Add(2 * time.Hour))
		}
	}()
	for range 100 {
		_, err := w.Write([]byte(`data: {"ok":true}` + "\n\n"))
		testutil.NoError(t, err)
		testutil.NoError(t, w.FlushError())
	}
	<-done
	testutil.NoError(t, w.Close())
	frames := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n\n"), "\n\n")
	count := 0
	for _, frame := range frames {
		if frame == `data: {"ok":true}` {
			count++
		} else {
			testutil.Equal(t, frame, ": keepalive")
		}
	}
	testutil.Equal(t, count, 100)
}

func TestAnthropicSSEKeepaliveUsesPingEvents(t *testing.T) {
	rec := httptest.NewRecorder()
	trace := middleware.NewTracedResponseWriter(rec)
	trace.Header().Set("Content-Type", "text/event-stream")
	w := NewAnthropicSSEKeepalive(trace, time.Hour, nil)
	w.WriteHeader(http.StatusOK)
	testutil.NoError(t, w.heartbeat(time.Now().Add(2*time.Hour)))
	testutil.NoError(t, w.Close())
	testutil.Equal(t, rec.Body.String(), "event: ping\ndata: {\"type\":\"ping\"}\n\n")
	testutil.True(t, trace.ContentWriteAt().IsZero())
}

func TestSSEKeepaliveFlushesBeforeFirstGeneratedEvent(t *testing.T) {
	allowOutput := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		stream := NewSSEKeepalive(w, 5*time.Millisecond, nil)
		defer stream.Close()
		stream.WriteHeader(http.StatusOK)
		select {
		case <-allowOutput:
			_, _ = stream.Write([]byte("data: generated\n\n"))
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	testutil.NoError(t, err)
	resp, err := server.Client().Do(req)
	testutil.NoError(t, err)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	testutil.NoError(t, err)
	testutil.Equal(t, line, ": keepalive\n")
	close(allowOutput)
	rest, err := io.ReadAll(reader)
	testutil.NoError(t, err)
	testutil.MustContain(t, string(rest), "data: generated\n\n")
}
