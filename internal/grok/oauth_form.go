package grok

import (
	"context"
	"net/http"
	"net/url"
	"orchids-api/internal/httpclient"
	"strings"
)

// OAuth endpoints share a bounded form transport, but keep their headers,
// decoding contracts and error classifiers separate. Incomplete successful
// responses cannot be used as durable OAuth credentials.
func postOAuthForm(ctx context.Context, client *http.Client, endpoint string, form url.Values, limit int64,
	prepare func(*http.Request), classify func([]byte, int) error) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if prepare != nil {
		prepare(req)
	}
	resp, body, err := httpclient.DoReadBody(client, req, limit)
	if resp != nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		return body, resp.StatusCode, classify(body, resp.StatusCode)
	}
	if err != nil {
		return nil, 0, err
	}
	return body, resp.StatusCode, nil
}
