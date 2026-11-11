package web

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"orchids-api/internal/httpclient"
)

// The outbound check lives in internal/httpclient so every provider that dials a
// client-supplied URL shares one implementation. These aliases keep this
// package's internal call sites and errors.Is checks unchanged.
var errRemoteFetchBlocked = httpclient.ErrRemoteFetchBlocked

func newRemoteFetchClient(timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	return httpclient.NewRemoteFetchClient(timeout, proxyFunc)
}

func checkRemoteFetchTarget(ctx context.Context, rawURL string, proxied bool) (*url.URL, error) {
	return httpclient.CheckRemoteFetchTarget(ctx, rawURL, proxied)
}
