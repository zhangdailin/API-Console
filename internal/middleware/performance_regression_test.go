package middleware

import (
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/testutil"
	"testing"
	"time"
)

func TestNestedAdmissionUsesOneGlobalSlot(t *testing.T) {
	limiter := NewConcurrencyLimiter(1, time.Second)
	handler := limiter.Limit(limiter.Limit(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest("POST", "/grok/v1/responses", nil))
	testutil.Equal(t, rec.Code, 204)
}
func TestProviderAdmissionSeparatesBudgetsAndReleases(t *testing.T) {
	wrap := ProviderAdmission(func() map[string]int { return map[string]int{"grok": 1, "cline": 1} })
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	h := wrap(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/grok/hold" {
			close(entered)
			<-release
		}
		w.WriteHeader(204)
	})
	go func() { defer close(done); h(httptest.NewRecorder(), httptest.NewRequest("POST", "/grok/hold", nil)) }()
	<-entered
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/grok/second", nil))
	testutil.Equal(t, rec.Code, 503)
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/cline/test", nil))
	testutil.Equal(t, rec.Code, 204)
	close(release)
	<-done
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/grok/after", nil))
	testutil.Equal(t, rec.Code, 204)
}
