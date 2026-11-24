//go:build ignore

package main

import (
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"time"

	"orchids-api/internal/httpclient"
)

func probe(rawURL string) error {
	transport := httpclient.GetSharedHTTPClientWithHTTP2("alpn-probe", 10*time.Second, nil, true).Transport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	if rawURL == "" {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		server.EnableHTTP2 = true
		server.StartTLS()
		defer server.Close()
		transport.TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
		rawURL = server.URL
	}
	target, err := url.Parse(rawURL)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil {
		return fmt.Errorf("provide an HTTPS URL without user information")
	}
	client := &http.Client{
		Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(target.String())
	if err != nil {
		return fmt.Errorf("HTTPS request failed: %w", err)
	}
	defer resp.Body.Close()
	fmt.Printf("protocol=%s alpn=%q status=%d\n", resp.Proto, resp.TLS.NegotiatedProtocol, resp.StatusCode)
	return nil
}

func main() {
	target := flag.String("url", "", "HTTPS endpoint (defaults to a local HTTP/2 fixture)")
	flag.Parse()
	if err := probe(*target); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
