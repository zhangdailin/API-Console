package debug

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// Latency uses one monotonic clock per attempt. Missing stages stay absent,
// notably DNS/TCP/TLS on a reused connection. Hooks never change the transport.
type Latency struct {
	mu      sync.Mutex
	attempt *UpstreamAttempt
	started time.Time
	values  map[string]any
	stages  map[string]time.Time
}

func (a *UpstreamAttempt) Trace(ctx context.Context, metadata map[string]any) (context.Context, *Latency) {
	if a == nil {
		return ctx, nil
	}
	l := &Latency{attempt: a, started: a.started, values: metadata, stages: map[string]time.Time{}}
	if l.values == nil {
		l.values = map[string]any{}
	}
	l.values["before_upstream_ms"] = a.started.Sub(a.capture.started).Milliseconds()
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { l.start("dns") },
		DNSDone: func(i httptrace.DNSDoneInfo) {
			l.end("dns")
			addresses := []string{}
			for _, a := range i.Addrs {
				addresses = append(addresses, a.String())
			}
			l.Set("resolved_ips", addresses)
		},
		ConnectStart: func(_, addr string) { l.start("tcp:" + addr) },
		ConnectDone: func(_, addr string, err error) {
			l.end("tcp:" + addr)
			if err == nil {
				l.Set("connected_address", addr)
			}
		},
		TLSHandshakeStart: func() { l.start("tls") },
		TLSHandshakeDone:  func(s tls.ConnectionState, _ error) { l.end("tls"); l.Set("alpn", s.NegotiatedProtocol) },
		GetConn:           func(string) { l.start("connection_wait") },
		GotConn: func(i httptrace.GotConnInfo) {
			l.end("connection_wait")
			l.Set("connection_reused", i.Reused)
			l.Set("connection_was_idle", i.WasIdle)
			l.Set("remote_address", i.Conn.RemoteAddr().String())
			l.Set("connection_idle_ms", i.IdleTime.Milliseconds())
		},
		WroteRequest:         func(httptrace.WroteRequestInfo) { l.Mark("request_written_ms") },
		GotFirstResponseByte: func() { l.Mark("first_response_byte_ms") },
	}
	return httptrace.WithClientTrace(ctx, trace), l
}
func (l *Latency) start(k string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.stages[k] = time.Now()
	l.mu.Unlock()
}
func (l *Latency) end(k string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	if t, ok := l.stages[k]; ok {
		l.values[k+"_ms"] = time.Since(t).Milliseconds()
	}
	l.mu.Unlock()
}
func (l *Latency) Mark(k string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	if _, ok := l.values[k]; !ok {
		l.values[k] = time.Since(l.started).Milliseconds()
	}
	l.mu.Unlock()
}
func (l *Latency) Set(k string, v any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.values[k] = v
	l.mu.Unlock()
}
func (l *Latency) Response(r *http.Response) {
	if l == nil || r == nil {
		return
	}
	l.Set("http_status", r.StatusCode)
	l.Set("http_protocol", r.Proto)
	l.Mark("response_headers_ms")
}
func (l *Latency) Finish(err error) {
	if l == nil {
		return
	}
	l.Mark("total_ms")
	if err != nil {
		l.Set("error", err.Error())
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempt.writeJSON("latency.json", l.values)
}

// RecordWait preserves each retry decision and the wait actually spent, including cancellation.
func RecordWait(ctx context.Context, reason string, planned, actual time.Duration, cancelled bool) {
	c := FromContext(ctx)
	if c == nil {
		return
	}
	a := beginUpstream(c, "WAIT", "", nil, nil)
	a.writeJSON("latency.json", map[string]any{"kind": "retry_wait", "reason": reason, "planned_wait_ms": planned.Milliseconds(), "actual_wait_ms": actual.Milliseconds(), "cancelled": cancelled})
}
