package httpclient

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"orchids-api/internal/testutil"
)

// A server that also speaks h2 must still get an HTTP/1.1 WebSocket upgrade:
// the dial presents a Chrome ClientHello that offers only http/1.1.
func TestDialBrowserWebSocketTLSUpgradesOverHTTP1(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte("hello "+r.Proto+" "+r.TLS.NegotiatedProtocol))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	browserWebSocketRootCAs = pool
	defer func() { browserWebSocketRootCAs = nil }()

	direct := func(*http.Request) (*url.URL, error) { return nil, nil }
	dialer := websocket.Dialer{NetDialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return DialBrowserWebSocketTLS(ctx, network, addr, direct, "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/133.0.0.0 Safari/537.36")
	}}
	conn, _, err := dialer.Dial("wss://"+strings.TrimPrefix(server.URL, "https://")+"/ws", nil)
	testutil.NoError(t, err)
	defer conn.Close()
	_, message, err := conn.ReadMessage()
	testutil.NoError(t, err)
	// The server offers h2; the dial must not take it.
	testutil.MustContain(t, string(message), "hello HTTP/1.1")
	testutil.MustNotContain(t, string(message), "h2")
}
