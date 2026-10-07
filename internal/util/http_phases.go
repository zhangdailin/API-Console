package util

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

type httpPhaseObserverKey struct{}
type HTTPPhaseObserver func(map[string]int64)

func WithHTTPPhaseObserver(ctx context.Context, observer HTTPPhaseObserver) context.Context {
	return context.WithValue(ctx, httpPhaseObserverKey{}, observer)
}

// TraceHTTPPhases records timings without bodies, URLs, addresses or credentials.
// Missing stages remain absent on reused connections or custom transports.
func TraceHTTPPhases(req *http.Request) *http.Request {
	observer, _ := req.Context().Value(httpPhaseObserverKey{}).(HTTPPhaseObserver)
	if observer == nil {
		return req
	}
	var mu sync.Mutex
	started := time.Now()
	starts := map[string]time.Time{}
	values := map[string]int64{}
	start := func(key string) { mu.Lock(); starts[key] = time.Now(); mu.Unlock() }
	end := func(key, name string) {
		mu.Lock()
		if t, ok := starts[key]; ok {
			values[name] = time.Since(t).Milliseconds()
		}
		mu.Unlock()
	}
	trace := &httptrace.ClientTrace{
		DNSStart:     func(httptrace.DNSStartInfo) { start("dns") },
		DNSDone:      func(httptrace.DNSDoneInfo) { end("dns", "dns_ms") },
		ConnectStart: func(_, addr string) { start("tcp:" + addr) },
		ConnectDone: func(_, addr string, err error) {
			if err == nil {
				end("tcp:"+addr, "tcp_ms")
			}
		},
		TLSHandshakeStart: func() { start("tls") },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { end("tls", "tls_ms") },
		GetConn:           func(string) { start("connection") },
		GotConn: func(info httptrace.GotConnInfo) {
			end("connection", "connection_wait_ms")
			mu.Lock()
			if info.Reused {
				values["reused_connections"] = 1
			}
			mu.Unlock()
		},
		GotFirstResponseByte: func() {
			mu.Lock()
			values["response_headers_ms"] = time.Since(started).Milliseconds()
			snapshot := make(map[string]int64, len(values))
			for k, v := range values {
				snapshot[k] = v
			}
			mu.Unlock()
			observer(snapshot)
		},
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
}
