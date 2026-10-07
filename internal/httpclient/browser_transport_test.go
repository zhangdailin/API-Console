package httpclient

import (
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"orchids-api/internal/testutil"
	"strconv"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

func TestBrowserHTTPClientCustomHeaderDeadlineIsIsolated(t *testing.T) {
	defaultClient := GetSharedBrowserHTTPClientWithHeaderTimeout("header-isolation-test", 10*time.Minute, responseHeaderTimeoutForClient(10*time.Minute), nil)
	longClient := GetSharedBrowserHTTPClientWithHeaderTimeout("header-isolation-test", 10*time.Minute, 0, nil)
	testutil.NotEqual(t, defaultClient, longClient)
	testutil.Equal(t, defaultClient.Transport.(*browserLikeRoundTripper).http1.ResponseHeaderTimeout, 2*time.Minute)
	testutil.Equal(t, longClient.Transport.(*browserLikeRoundTripper).http1.ResponseHeaderTimeout, 0)
	testutil.Equal(t, longClient.Timeout, 10*time.Minute)
	testutil.Equal(t, GetSharedBrowserHTTPClientWithHeaderTimeout("header-isolation-test", 10*time.Minute, 0, nil), longClient)
}

func TestDialHTTPSProxyAwareSupportsSOCKS5(t *testing.T) {
	target := startEchoListener(t)
	proxy := startSOCKS5Proxy(t)

	proxyURL, err := url.Parse("socks5://" + proxy.Addr().String())
	testutil.NoError(t, err, "parse proxy url: %v")
	proxyFunc := func(*http.Request) (*url.URL, error) { return proxyURL, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, targetHost, err := dialHTTPSProxyAware(ctx, "tcp", target.Addr().String(), proxyFunc)
	testutil.NoError(t, err, "dialHTTPSProxyAware() error = %v")
	defer conn.Close()
	testutil.NotEqual(t, targetHost, "")
	_, err = conn.Write([]byte("ping"))
	testutil.CheckNoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	testutil.CheckNoError(t, err)
	testutil.Equal(t, string(buf), "ping")
}

func TestDialHTTPSProxyAwareSupportsHTTPSProxy(t *testing.T) {
	proxyDone := make(chan struct{})
	connectTarget := make(chan string, 1)
	connectAuth := make(chan string, 1)
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		connectTarget <- r.Host
		connectAuth <- r.Header.Get("Proxy-Authorization")
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		_, _ = io.Copy(conn, conn)
		close(proxyDone)
	}))
	proxy.StartTLS()
	t.Cleanup(func() {
		proxy.Close()
	})

	proxyURL, err := url.Parse(proxy.URL)
	testutil.NoError(t, err)
	proxyURL.User = url.UserPassword("proxy-user", "proxy-secret")
	roots := x509.NewCertPool()
	roots.AddCert(proxy.Certificate())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, targetHost, err := dialHTTPSProxyTunnel(ctx, "tcp", "upstream.example:443", "upstream.example", proxyURL, &stdtls.Config{RootCAs: roots, ServerName: proxyURL.Hostname(), MinVersion: stdtls.VersionTLS12})
	testutil.NoError(t, err, "HTTPS proxy dial failed")
	defer conn.Close()
	testutil.Equal(t, targetHost, "upstream.example")
	testutil.Equal(t, <-connectTarget, "upstream.example:443")
	testutil.Equal(t, <-connectAuth, "Basic "+base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy-secret")))
	testutil.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = conn.Write([]byte("ping"))
	testutil.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	testutil.NoError(t, err)
	testutil.Equal(t, string(buf), "ping")
	testutil.NoError(t, conn.Close())
	select {
	case <-proxyDone:
	case <-ctx.Done():
		t.Fatal("HTTPS proxy tunnel did not close")
	}
}

func TestDialHTTPSProxyAwareRejectsUntrustedHTTPSProxy(t *testing.T) {
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("untrusted proxy must not receive a CONNECT request")
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	testutil.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := dialHTTPSProxyAware(ctx, "tcp", "upstream.example:443", http.ProxyURL(proxyURL))
	if conn != nil {
		conn.Close()
	}
	var untrusted x509.UnknownAuthorityError
	testutil.True(t, errors.As(err, &untrusted), "HTTPS proxy certificate must be verified")
}

func startEchoListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	testutil.NoError(t, err, "listen echo: %v")
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln
}

func startSOCKS5Proxy(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	testutil.NoError(t, err, "listen socks5: %v")
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSOCKS5ProxyConn(conn)
		}
	}()
	return ln
}

func handleSOCKS5ProxyConn(conn net.Conn) {
	defer conn.Close()
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}
	if header[0] != 5 {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
	if req[0] != 5 || req[1] != 1 {
		return
	}
	host := ""
	switch req[3] {
	case 1:
		addr := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return
		}
		host = net.IP(addr).String()
	case 3:
		size := make([]byte, 1)
		if _, err := io.ReadFull(conn, size); err != nil {
			return
		}
		addr := make([]byte, int(size[0]))
		if _, err := io.ReadFull(conn, addr); err != nil {
			return
		}
		host = string(addr)
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return
	}
	port := int(portBytes[0])<<8 | int(portBytes[1])
	upstream, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, conn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		done <- struct{}{}
	}()
	<-done
}

func TestUtlsProfileFollowsUserAgentChromeVersion(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36":       "133",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36": "131",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36":                 "120",
		// Older than any shipped profile: the oldest profile, never the newest.
		"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/100.0.0.0 Safari/537.36": "120",
	}
	for ua, want := range cases {
		profile := utlsProfileForUserAgent(ua)
		testutil.Equal(t, profile.Version, want)
	}
	// A UA without a Chrome version keeps the library default.
	testutil.Equal(t, utlsProfileForUserAgent("curl/8.0"), utls.HelloChrome_Auto)
	testutil.Equal(t, chromeMajorFromUserAgent("curl/8.0"), 0)
}
