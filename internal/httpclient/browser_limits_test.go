package httpclient

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"orchids-api/internal/config"
)

func testH2Browser(t *testing.T, handler http.Handler, headerTimeout time.Duration) (*http.Client, *httptest.Server) {
	t.Helper()
	s := httptest.NewUnstartedServer(handler)
	s.EnableHTTP2 = true
	if err := http2.ConfigureServer(s.Config, &http2.Server{MaxConcurrentStreams: 1}); err != nil {
		t.Fatal(err)
	}
	s.StartTLS()
	t.Cleanup(s.Close)
	c := GetSharedBrowserHTTPClientWithLimits(t.Name(), 2*time.Second, headerTimeout, nil, &config.Config{UpstreamMaxConnsPerHost: 1, UpstreamMaxIdleConnsPerHost: 1})
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	rt := c.Transport.(*browserLikeRoundTripper)
	rt.http2.TLSClientConfig.RootCAs = roots
	t.Cleanup(c.CloseIdleConnections)
	return c, s
}

func TestBrowserH2HeaderTimeoutCancelsStalledResponse(t *testing.T) {
	c, s := testH2Browser(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }), 40*time.Millisecond)
	started := time.Now()
	_, err := c.Get(s.URL)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("headers did not time out promptly: %v", err)
	}
}

func TestBrowserH2HeaderTimerLeavesStreamingBodyAlive(t *testing.T) {
	c, s := testH2Browser(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		io.WriteString(w, "ok")
	}), 40*time.Millisecond)
	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || string(raw) != "ok" || resp.ProtoMajor != 2 {
		t.Fatalf("stream truncated: protocol=%s body=%q err=%v", resp.Proto, raw, err)
	}
}

func TestBrowserH2ConnectionLimitDoesNotDeadlockStreamWaiters(t *testing.T) {
	c, s := testH2Browser(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(15 * time.Millisecond)
		io.WriteString(w, "ok")
	}), time.Second)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.Get(s.URL)
			if err == nil {
				_, err = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestH2SocketAdmissionWaitHonorsCancellation(t *testing.T) {
	limits := &h2ConnectionLimits{max: 1}
	release, err := limits.acquire(context.Background(), "host:443")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = limits.acquire(ctx, "host:443")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("socket waiter error=%v", err)
	}
	release()
	limits.mu.Lock()
	defer limits.mu.Unlock()
	if len(limits.hosts) != 0 {
		t.Fatal("socket admission retained an idle gate")
	}
}
