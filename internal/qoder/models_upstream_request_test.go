package qoder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/testutil"
)

type catalogTransport func(*http.Request) (*http.Response, error)

func (fn catalogTransport) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestCatalogReadReturnsFirstFailureWithoutProbing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", 401, `{"message":"original auth failure"}`},
		{"forbidden", 403, `{"message":"original forbidden failure"}`},
		{"not found", 404, `{"message":"original route failure"}`},
		{"parse failure", 200, `{"message":"original parse failure"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewFromAccount(signedTestAccount(), nil)
			calls := 0
			client.control = &http.Client{Transport: catalogTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				testutil.Equal(t, req.Method, http.MethodGet)
				testutil.Equal(t, req.URL.RequestURI(), modelListPath)
				testutil.True(t, req.Body == nil, "catalog read carried a body")
				testutil.Equal(t, req.Header.Get("Accept"), "application/json")
				testutil.True(t, strings.HasPrefix(req.Header.Get("Authorization"), "Bearer COSY."), "catalog read was unsigned")
				if calls > 1 {
					return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"message":"later failure"}`))}, nil
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			catalog, err := client.FetchUpstreamModels(context.Background())
			testutil.Error(t, err)
			testutil.True(t, catalog == nil, "failed read returned a catalog")
			testutil.Equal(t, calls, 1)
			if tc.status != http.StatusOK {
				want := apiError(http.MethodGet, client.endpoints.inference+modelListPath, tc.status, []byte(tc.body))
				testutil.Equal(t, err.Error(), want.Error())
			} else {
				testutil.Equal(t, err.Error(), "GET /api/v2/model/list: upstream reported: original parse failure")
			}
		})
	}
}

func TestCatalogReadPreservesTransportFailure(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "network", true: "timeout"}[timeout], func(t *testing.T) {
			client := NewFromAccount(signedTestAccount(), nil)
			first := errors.New("original network interruption")
			calls := 0
			client.control = &http.Client{Transport: catalogTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if timeout {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				return nil, first
			})}
			ctx := context.Background()
			if timeout {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
				first = context.DeadlineExceeded
			}
			_, err := client.FetchUpstreamModels(ctx)
			testutil.True(t, errors.Is(err, first), "original transport cause was lost")
			testutil.True(t, errors.Is(err, ErrAuthUnavailable), "auth availability classification was lost")
			testutil.Equal(t, calls, 1)
		})
	}
}
