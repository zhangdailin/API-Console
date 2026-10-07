package httpclient

import (
	"bufio"
	"context"
	stdtls "crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"orchids-api/internal/config"
	"strconv"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	netproxy "golang.org/x/net/proxy"
)

type browserLikeRoundTripper struct {
	http1 *http.Transport
	http2 *http2.Transport
	// hello is the TLS ClientHello presented on every connection of this
	// transport. It is chosen from the User-Agent the caller will send, so the
	// fingerprint and the advertised browser version cannot contradict each
	// other — a mismatch is one of the most reliable bot signals.
	hello utls.ClientHelloID
}

func (rt *browserLikeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && strings.EqualFold(req.URL.Scheme, "https") {
		return rt.http2.RoundTrip(req)
	}
	return rt.http1.RoundTrip(req)
}

func (rt *browserLikeRoundTripper) CloseIdleConnections() {
	if rt.http1 != nil {
		rt.http1.CloseIdleConnections()
	}
	if rt.http2 != nil {
		rt.http2.CloseIdleConnections()
	}
}

var browserHTTPClientCache = clientPool{clients: make(map[string]*http.Client)}

// A zero headerTimeout leaves header waiting bounded by the HTTP total
// deadline. Keep it in the cache key so custom long-lived inference clients
// cannot inherit another caller's shorter HTTP/1 header deadline.
func GetSharedBrowserHTTPClientWithHeaderTimeout(proxyKey string, timeout, headerTimeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	return getSharedBrowserHTTPClient(proxyKey, timeout, headerTimeout, proxyFunc, "")
}

func GetSharedBrowserHTTPClientWithLimits(proxyKey string, timeout, headerTimeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error), cfg *config.Config) *http.Client {
	return getSharedBrowserHTTPClient(proxyKey, timeout, headerTimeout, proxyFunc, "", cfg)
}

func getSharedBrowserHTTPClient(proxyKey string, timeout, headerTimeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error), userAgent string, configs ...*config.Config) *http.Client {
	if proxyKey == "" {
		proxyKey = "direct"
	}
	hello := utlsProfileForUserAgent(userAgent)
	cacheKey := "browser|" + sharedHTTPClientCacheKey(proxyKey, timeout) + fmt.Sprintf("|headers=%d|hello=%s", headerTimeout, hello.Version)
	var cfg *config.Config
	if len(configs) > 0 {
		cfg = configs[0]
	}
	maxConns, maxIdle := HTTPPoolLimits(cfg)
	cacheKey += fmt.Sprintf("|connections=%d|idle=%d", maxConns, maxIdle)

	browserHTTPClientCache.mu.RLock()
	client, ok := browserHTTPClientCache.clients[cacheKey]
	browserHTTPClientCache.mu.RUnlock()
	if ok {
		return client
	}

	browserHTTPClientCache.mu.Lock()
	defer browserHTTPClientCache.mu.Unlock()
	if client, ok = browserHTTPClientCache.clients[cacheKey]; ok {
		return client
	}

	rt := &browserLikeRoundTripper{
		hello: hello,
		http1: &http.Transport{
			Proxy:                 proxyFunc,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          maxIdle,
			MaxIdleConnsPerHost:   maxIdle,
			MaxConnsPerHost:       maxConns,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: headerTimeout,
			TLSClientConfig:       &stdtls.Config{MinVersion: stdtls.VersionTLS12},
		},
		http2: &http2.Transport{
			AllowHTTP:       false,
			ReadIdleTimeout: 20 * time.Second,
			PingTimeout:     10 * time.Second,
			DialTLSContext: func(ctx context.Context, network, addr string, cfg *stdtls.Config) (net.Conn, error) {
				return dialUTLSHTTP2ContextWithHello(ctx, network, addr, cfg, proxyFunc, hello)
			},
			TLSClientConfig: &stdtls.Config{
				MinVersion: stdtls.VersionTLS12,
				NextProtos: []string{"h2"},
			},
		},
	}

	client = &http.Client{
		Transport: rt,
		Timeout:   timeout,
	}
	browserHTTPClientCache.evictOneClientLocked()
	browserHTTPClientCache.clients[cacheKey] = client
	return client
}

func dialUTLSHTTP2ContextWithHello(ctx context.Context, network, addr string, cfg *stdtls.Config, proxyFunc func(*http.Request) (*url.URL, error), hello utls.ClientHelloID) (net.Conn, error) {
	rawConn, targetHost, err := dialHTTPSProxyAware(ctx, network, addr, proxyFunc)
	if err != nil {
		return nil, err
	}

	serverName := targetHost
	if cfg != nil && strings.TrimSpace(cfg.ServerName) != "" {
		serverName = strings.TrimSpace(cfg.ServerName)
	}
	utlsCfg := &utls.Config{
		ServerName: serverName,
		MinVersion: utls.VersionTLS12,
		NextProtos: []string{"h2"},
	}
	if hello == (utls.ClientHelloID{}) {
		hello = utls.HelloChrome_Auto
	}
	conn := utls.UClient(rawConn, utlsCfg, hello)
	if err := conn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, err
	}
	if proto := conn.ConnectionState().NegotiatedProtocol; proto != http2.NextProtoTLS {
		conn.Close()
		return nil, fmt.Errorf("browser http2: unexpected ALPN protocol %q", proto)
	}
	return conn, nil
}

func dialHTTPSProxyAware(ctx context.Context, network, addr string, proxyFunc func(*http.Request) (*url.URL, error)) (net.Conn, string, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, "", err
	}

	if proxyFunc == nil {
		conn, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
		return conn, host, err
	}

	req := &http.Request{URL: &url.URL{Scheme: "https", Host: addr}}
	proxyURL, err := proxyFunc(req)
	if err != nil {
		return nil, "", err
	}
	if proxyURL == nil {
		conn, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
		return conn, host, err
	}
	switch strings.ToLower(strings.TrimSpace(proxyURL.Scheme)) {
	case "", "http":
		return dialHTTPProxyTunnel(ctx, network, addr, host, proxyURL)
	case "socks5", "socks5h":
		conn, err := dialSOCKS5Proxy(ctx, network, addr, proxyURL)
		return conn, host, err
	case "https":
		return dialHTTPSProxyTunnel(ctx, network, addr, host, proxyURL, nil)
	default:
		return nil, "", fmt.Errorf("browser http2 proxy scheme %q is not supported", proxyURL.Scheme)
	}
}

func dialHTTPSProxyTunnel(ctx context.Context, network, addr, targetHost string, proxyURL *url.URL, tlsConfig *stdtls.Config) (net.Conn, string, error) {
	proxyAddr := proxyURL.Host
	if _, _, err := net.SplitHostPort(proxyAddr); err != nil {
		proxyAddr = net.JoinHostPort(proxyURL.Hostname(), "443")
	}
	forward := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	rawConn, err := forward.DialContext(ctx, network, proxyAddr)
	if err != nil {
		return nil, "", err
	}
	config := &stdtls.Config{
		ServerName: proxyURL.Hostname(),
		MinVersion: stdtls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	}
	if tlsConfig != nil {
		config = tlsConfig
	}
	// Streaming clients can have no overall deadline; still bound the proxy's
	// TLS handshake so a stalled proxy cannot hold a dial forever.
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = rawConn.SetDeadline(deadline)
	tlsConn := stdtls.Client(rawConn, config)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = rawConn.Close()
		return nil, "", err
	}
	_ = tlsConn.SetDeadline(time.Time{})
	if err := writeHTTPConnect(ctx, tlsConn, addr, proxyURL); err != nil {
		_ = tlsConn.Close()
		return nil, "", err
	}
	return tlsConn, targetHost, nil
}

func dialHTTPProxyTunnel(ctx context.Context, network, addr, targetHost string, proxyURL *url.URL) (net.Conn, string, error) {
	proxyAddr := proxyURL.Host
	if _, _, splitErr := net.SplitHostPort(proxyAddr); splitErr != nil {
		proxyAddr = net.JoinHostPort(proxyAddr, "80")
	}
	conn, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, proxyAddr)
	if err != nil {
		return nil, "", err
	}
	if err := writeHTTPConnect(ctx, conn, addr, proxyURL); err != nil {
		conn.Close()
		return nil, "", err
	}
	return conn, targetHost, nil
}

func dialSOCKS5Proxy(ctx context.Context, network, addr string, proxyURL *url.URL) (net.Conn, error) {
	proxyAddr := strings.TrimSpace(proxyURL.Host)
	if proxyAddr == "" {
		return nil, fmt.Errorf("socks5 proxy host is empty")
	}
	if _, _, splitErr := net.SplitHostPort(proxyAddr); splitErr != nil {
		proxyAddr = net.JoinHostPort(proxyAddr, "1080")
	}

	var auth *netproxy.Auth
	if proxyURL.User != nil {
		password := ""
		if pass, ok := proxyURL.User.Password(); ok {
			password = pass
		}
		auth = &netproxy.Auth{
			User:     proxyURL.User.Username(),
			Password: password,
		}
	}
	forward := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	dialer, err := netproxy.SOCKS5(network, proxyAddr, auth, forward)
	if err != nil {
		return nil, err
	}
	if ctxDialer, ok := dialer.(netproxy.ContextDialer); ok {
		return ctxDialer.DialContext(ctx, network, addr)
	}
	return dialer.Dial(network, addr)
}

func writeHTTPConnect(ctx context.Context, conn net.Conn, target string, proxyURL *url.URL) error {
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	defer conn.SetDeadline(time.Time{})

	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: make(http.Header),
	}
	if proxyURL != nil && proxyURL.User != nil {
		user := proxyURL.User.Username()
		password := ""
		if pass, ok := proxyURL.User.Password(); ok {
			password = pass
		}
		token := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
		connectReq.Header.Set("Proxy-Authorization", "Basic "+token)
	}
	if err := connectReq.Write(conn); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), connectReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy CONNECT failed: %s", resp.Status)
	}
	return nil
}

// utlsProfileForUserAgent picks the TLS ClientHello whose Chrome version is
// closest to (but never newer than) the version the User-Agent claims. A UA
// that does not name a Chrome version keeps the library default.
//
// The available profiles are the ones this utls release ships; a UA claiming a
// newer Chrome than any profile gets the newest profile, which is the closest
// honest approximation.
func utlsProfileForUserAgent(userAgent string) utls.ClientHelloID {
	major := chromeMajorFromUserAgent(userAgent)
	if major <= 0 {
		return utls.HelloChrome_Auto
	}
	// This utls release ships three Chrome profiles. A UA claiming a newer
	// version than any profile gets the newest one (the closest honest
	// approximation); an older claim gets the oldest, rather than the default
	// which would advertise the newest Chrome.
	profiles := [...]struct {
		major int
		id    utls.ClientHelloID
	}{
		{133, utls.HelloChrome_133},
		{131, utls.HelloChrome_131},
		{120, utls.HelloChrome_120},
	}
	best := profiles[len(profiles)-1].id
	for _, profile := range profiles {
		if profile.major <= major {
			best = profile.id
			break
		}
	}
	return best
}

// chromeMajorFromUserAgent extracts the major version from a Chrome UA.
func chromeMajorFromUserAgent(userAgent string) int {
	marker := "Chrome/"
	index := strings.Index(userAgent, marker)
	if index < 0 {
		return 0
	}
	rest := userAgent[index+len(marker):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	major, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return major
}
