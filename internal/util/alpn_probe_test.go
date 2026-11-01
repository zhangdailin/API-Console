//go:build live

package util

// Opt-in probes for the shared transport's TLS/ALPN behaviour. They make real
// network connections, so they require -tags live and the matching environment
// variable to be set:
//
//	TLS_PROBE_HOST=example.com:443   -> what ALPN the shared transport offers
//	TLS_PROBE_H2_LOCAL=127.0.0.1:8443  -> what protocol it speaks to a local h2 server

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/testutil"
	"os"
	"strings"
	"testing"
	"time"
)

// TestTLSALPNToRealHost reports the ALPN the shared TLS config negotiates with a
// real host, and what protocol the shared HTTP client then uses.
func TestTLSALPNToRealHost(t *testing.T) {
	host := os.Getenv("TLS_PROBE_HOST")
	if host == "" {
		t.Skip("set TLS_PROBE_HOST=host:port")
	}
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).Dial("tcp", host)
	testutil.Falsef(t, err != nil, "dial %s: %v", host, err)
	defer conn.Close()
	tlsConn := tls.Client(conn, &tls.Config{ServerName: strings.Split(host, ":")[0]})
	err := tlsConn.Handshake()
	testutil.CheckNoError(t, err)
	state := tlsConn.ConnectionState()
	fmt.Printf("ALPN host=%s negotiated=%q version=%#x\n", host, state.NegotiatedProtocol, state.Version)

	client := GetSharedHTTPClient("alpn-probe", 30*time.Second, nil)
	req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/", nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("ALPN client_error=%v\n", err)
		return
	}
	defer resp.Body.Close()
	fmt.Printf("ALPN client_proto=%s status=%d\n", resp.Proto, resp.StatusCode)
}

// TestSharedTransportProtocolAgainstLocalH2 starts a local HTTP/2 server and
// reports which protocol the shared transport uses against it.
func TestSharedTransportProtocolAgainstLocalH2(t *testing.T) {
	if os.Getenv("TLS_PROBE_H2_LOCAL") == "" {
		t.Skip("set TLS_PROBE_H2_LOCAL to run the local h2 probe")
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"proto": r.Proto,
			"alpn":  r.TLS.NegotiatedProtocol,
		})
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	client := GetSharedHTTPClient("alpn-probe-local", 30*time.Second, nil)
	if tr, ok := client.Transport.(*http.Transport); ok {
		tr = tr.Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // local self-signed test server
		client = &http.Client{Transport: tr, Timeout: 30 * time.Second}
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	testutil.NoError(t, err, "local h2 request: %v")
	defer resp.Body.Close()
	var out map[string]string
	testutil.NoError(t, json.NewDecoder(resp.Body).Decode(&out), "decode: %v")
	fmt.Printf("LOCALH2 client_proto=%s server_saw=%s alpn=%q\n", resp.Proto, out["proto"], out["alpn"])
}
