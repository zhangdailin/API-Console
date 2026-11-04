package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

func adminWrite(contentType, origin string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://admin.example/api/grok/admin/media/inputs/upload", strings.NewReader("x"))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Origin", origin)
	return req
}

func TestCookieWritesAcceptMultipartOnlyOnUploadRoutes(t *testing.T) {
	const multipart = "multipart/form-data; boundary=abc"
	rec := httptest.NewRecorder()
	testutil.False(t, CheckAdminCookieWrite(rec, adminWrite(multipart, "http://admin.example")))
	testutil.Equal(t, rec.Code, http.StatusUnsupportedMediaType)

	allowed := false
	handler := AllowMultipartAdminWrite(func(w http.ResponseWriter, r *http.Request) {
		allowed = CheckAdminCookieWrite(w, r)
	})
	handler(httptest.NewRecorder(), adminWrite(multipart, "http://admin.example"))
	testutil.True(t, allowed)

	// The same-origin proof still applies to an upload route.
	rec = httptest.NewRecorder()
	handler(rec, adminWrite(multipart, "https://evil.example"))
	testutil.False(t, allowed)
	testutil.Equal(t, rec.Code, http.StatusForbidden)

	// Only multipart is added: other types stay refused.
	rec = httptest.NewRecorder()
	handler(rec, adminWrite("text/plain", "http://admin.example"))
	testutil.Equal(t, rec.Code, http.StatusUnsupportedMediaType)
}
