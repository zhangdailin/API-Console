package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func newTestStore(t *testing.T, prefix string) (*store.Store, *miniredis.Miniredis) {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), CredentialEncryptionKey: []byte("01234567890123456789012345678901"), RedisPrefix: prefix})
	testutil.NoError(t, err, "store.New() error = %v")
	t.Cleanup(func() { _ = s.Close() })
	return s, mini
}

// channelLoginRequest builds the console's own request for a device-login
// endpoint: a localhost Origin so the origin guard accepts it, and a JSON body
// when there is one. Every channel login test needs exactly this shape, so the
// cline, qoder and workbuddy suites share the one builder.
func channelLoginRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "localhost"
	req.Header.Set("Origin", "http://localhost")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}
