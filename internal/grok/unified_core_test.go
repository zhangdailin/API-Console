package grok

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
	"time"
)

func TestUnifiedHTTPResponseLifecycle(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://upstream.test", nil)
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			_, _ = writer.Write([]byte(`{"message":"preserved"}`))
			_ = writer.Close()
			released := 0
			resp, err := doUpstreamHTTP(req, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Encoding": {"gzip"}, "Retry-After": {"7"}}, Body: &grokbase.LeaseBody{ReadCloser: io.NopCloser(&compressed), Release: func() { released++ }}}, nil
			}, time.Second)
			testutil.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			testutil.Falsef(t, err != nil || string(body) != `{"message":"preserved"}` || resp.StatusCode != status || resp.Header.Get("Retry-After") != "7" || released != 1 || resp.Header.Get("Content-Encoding") != "", "body=%s status=%d released=%d error=%v", body, resp.StatusCode, released, err)
		})
	}
	released := 0
	resp, err := doUpstreamHTTP(req, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {"gzip"}}, Body: &grokbase.LeaseBody{ReadCloser: io.NopCloser(strings.NewReader("invalid gzip")), Release: func() { released++ }}}, nil
	}, 0)
	testutil.Falsef(t, resp != nil || err == nil || released != 1, "decode failure leaked response: resp=%v err=%v released=%d", resp, err, released)
	wantErr := errors.New("transport failure")
	_, err = doUpstreamHTTP(req, func(*http.Request) (*http.Response, error) { return nil, wantErr }, 0)
	testutil.Falsef(t, !errors.Is(err, wantErr), "transport error changed: %v", err)
}
