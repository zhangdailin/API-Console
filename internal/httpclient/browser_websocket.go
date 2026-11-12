package httpclient

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	utls "github.com/refraction-networking/utls"
)

// browserWebSocketRootCAs replaces the system roots in tests only.
var browserWebSocketRootCAs *x509.CertPool

// DialBrowserWebSocketTLS dials addr through proxyFunc and completes a TLS
// handshake that presents the Chrome ClientHello matching userAgent, as the
// HTTP transport does, but offers only http/1.1 in ALPN: a browser opens a
// WebSocket on an HTTP/1.1 connection, and a server that negotiated h2 could
// not upgrade it. Use it as a WebSocket dialer's TLS dial function.
func DialBrowserWebSocketTLS(ctx context.Context, network, addr string, proxyFunc func(*http.Request) (*url.URL, error), userAgent string) (net.Conn, error) {
	rawConn, targetHost, err := dialHTTPSProxyAware(ctx, network, addr, proxyFunc)
	if err != nil {
		return nil, err
	}
	spec, err := utls.UTLSIdToSpec(utlsProfileForUserAgent(userAgent))
	if err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("browser websocket tls: %w", err)
	}
	for _, extension := range spec.Extensions {
		if alpn, ok := extension.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{"http/1.1"}
		}
	}
	conn := utls.UClient(rawConn, &utls.Config{ServerName: targetHost, MinVersion: utls.VersionTLS12, RootCAs: browserWebSocketRootCAs}, utls.HelloCustom)
	if err := conn.ApplyPreset(&spec); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("browser websocket tls: %w", err)
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(handshakeCtx); err != nil {
		rawConn.Close()
		return nil, err
	}
	if proto := conn.ConnectionState().NegotiatedProtocol; proto != "" && proto != "http/1.1" {
		conn.Close()
		return nil, fmt.Errorf("browser websocket tls: unexpected ALPN protocol %q", proto)
	}
	return conn, nil
}
