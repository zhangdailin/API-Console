package util

import (
	"context"
	"crypto/tls"
	"maps"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

type httpPhaseObserverKey struct{}
type httpPhaseFinishKey struct{}
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
	var published bool
	publish := func() {
		mu.Lock()
		if published {
			mu.Unlock()
			return
		}
		published = true
		snapshot := make(map[string]int64, len(values))
		maps.Copy(snapshot, values)
		mu.Unlock()
		observer(snapshot)
	}
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
			mu.Unlock()
			publish()
		},
	}
	ctx := context.WithValue(req.Context(), httpPhaseFinishKey{}, publish)
	return req.WithContext(httptrace.WithClientTrace(ctx, trace))
}

// FinishHTTPPhases publishes completed connection stages when an attempt fails
// before headers. A successful header snapshot is never counted twice.
func FinishHTTPPhases(req *http.Request) {
	if finish, ok := req.Context().Value(httpPhaseFinishKey{}).(func()); ok {
		finish()
	}
}
