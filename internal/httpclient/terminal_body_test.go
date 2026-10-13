package httpclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

func TestTerminalDrainBoundsSlowAndExcessiveTails(t *testing.T) {
	read, write := io.Pipe()
	defer write.Close()
	resp := &http.Response{ProtoMajor: 1, Body: read}
	start := time.Now()
	DrainTerminalResponse(t.Context(), resp)
	if time.Since(start) > 2*time.Second {
		t.Fatal("terminal cleanup exceeded its independent budget")
	}
	if _, err := write.Write([]byte("tail")); err == nil {
		t.Fatal("slow body was not closed")
	}
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
	for i := 0; i < 2; i++ {
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

func TestTerminalDrainCancellationAndHTTP2Skip(t *testing.T) {
	for _, protocol := range []int{1, 2} {
		body := &countedTerminalBody{Reader: strings.NewReader("tail")}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if protocol == 2 {
			ctx = t.Context()
		}
		DrainTerminalResponse(ctx, &http.Response{ProtoMajor: protocol, Body: body})
		if body.bytes != 0 {
			t.Fatal("cancelled or HTTP/2 response was drained")
		}
	}
	read, write := io.Pipe()
	defer write.Close()
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	DrainTerminalResponse(ctx, &http.Response{ProtoMajor: 1, Body: read})
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("cancellation did not interrupt terminal cleanup")
	}
}
