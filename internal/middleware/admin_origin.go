package middleware

import (
	"context"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

type multipartAdminWriteKey struct{}

// AllowMultipartAdminWrite marks an admin route that accepts file uploads, so
// a cookie-authenticated write may carry multipart/form-data besides JSON. The
// same-origin proof is still required: it is what stops a cross-site form.
func AllowMultipartAdminWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(context.WithValue(r.Context(), multipartAdminWriteKey{}, true)))
	}
}

// CheckAdminCookieWrite requires an explicit same-origin proof for browser
// mutations. Header-authenticated clients do not rely on ambient credentials.
func CheckAdminCookieWrite(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin == nil || !strings.EqualFold(origin.Scheme, scheme) || !strings.EqualFold(origin.Host, r.Host) ||
		origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" ||
		strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		http.Error(w, "Admin writes require the same origin", http.StatusForbidden)
		return false
	}
	if r.Body != nil && (r.ContentLength != 0 || r.Header.Get("Content-Type") != "") {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		multipartAllowed, _ := r.Context().Value(multipartAdminWriteKey{}).(bool)
		if err != nil || !(strings.EqualFold(media, "application/json") || multipartAllowed && strings.EqualFold(media, "multipart/form-data")) {
			http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
			return false
		}
	}
	return true
}
