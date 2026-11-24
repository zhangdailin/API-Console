package httpclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestTerminalDrainInterruptsBlockedReader(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cancelIn   time.Duration
		maxElapsed time.Duration
	}{
		{"cleanup_deadline", 0, 2 * time.Second},
		{"request_cancelled_during_read", 20 * time.Millisecond, 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				read, write := io.Pipe()
				defer read.Close()
				defer write.Close()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.cancelIn > 0 {
					timer := time.AfterFunc(tc.cancelIn, cancel)
					defer timer.Stop()
				}

				start := time.Now()
				DrainTerminalResponse(ctx, &http.Response{ProtoMajor: 1, Body: read})
				if elapsed := time.Since(start); elapsed == 0 || elapsed > tc.maxElapsed {
					t.Fatalf("blocked terminal read returned after %s, want a bounded wait", elapsed)
				}
				if _, err := write.Write([]byte("tail")); err == nil {
					t.Fatal("blocked response body was not closed")
				}
			})
		})
	}
}

func TestTerminalDrainBoundsExcessiveTail(t *testing.T) {
	body := &countedTerminalBody{Reader: strings.NewReader(strings.Repeat("x", 512<<10))}
	DrainTerminalResponse(t.Context(), &http.Response{ProtoMajor: 1, Body: body})
	if body.bytes > (256<<10)+1 || !body.closed {
		t.Fatal("terminal cleanup exceeded byte budget or kept an incomplete body open")
	}
}

type countedTerminalBody struct {
	io.Reader
	bytes  int
	closed bool
}

func (b *countedTerminalBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.bytes += n
	return n, err
}
func (b *countedTerminalBody) Close() error { b.closed = true; return nil }

func TestTerminalDrainPreservesHTTP1ConnectionReuse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "terminal\nsmall-tail") }))
	defer server.Close()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	var reused bool
	for range 2 {
		ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }})
		r, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(resp.Body, make([]byte, len("terminal\n"))); err != nil {
			t.Fatal(err)
		}
		DrainTerminalResponse(ctx, resp)
		resp.Body.Close()
	}
	if !reused {
		t.Fatal("small terminal tails discarded HTTP/1 keepalive")
	}
}

func TestTerminalDrainReadRules(t *testing.T) {
	const tail = 192 << 10
	for _, tc := range []struct {
		name             string
		protocol         int
		cancelled, close bool
		wantBytes        int
	}{
		{"complete_http1_tail", 1, false, false, tail},
		{"http2_stream", 2, false, false, 0},
		{"already_cancelled", 1, true, false, 0},
		{"connection_close", 1, false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			body := &countedTerminalBody{Reader: strings.NewReader(strings.Repeat("x", tail))}
			DrainTerminalResponse(ctx, &http.Response{ProtoMajor: tc.protocol, Close: tc.close, Body: body})
			if body.bytes != tc.wantBytes {
				t.Fatalf("read %d tail bytes, want %d", body.bytes, tc.wantBytes)
			}
		})
	}
}
