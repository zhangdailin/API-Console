package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/testutil"
	"testing"
	"time"
)

func TestLimiterRejectsImmediatelyWhenBusyWithOpenAIError(t *testing.T) {
	cl := NewConcurrencyLimiter(1, time.Second)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	h := cl.Limit(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})

	go func() {
		defer close(done)
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://x/", nil))
	}()
	<-entered

	recorder := httptest.NewRecorder()
	start := time.Now()
	h(recorder, httptest.NewRequest(http.MethodGet, "http://x/", nil))
	elapsed := time.Since(start)

	testutil.Falsef(t, elapsed >= 100*time.Millisecond, "overloaded request waited %s; want immediate rejection", elapsed)
	testutil.Equal(t, recorder.Code, http.StatusServiceUnavailable)
	testutil.Equal(t, recorder.Header().Get("Content-Type"), "application/json")
	testutil.Equal(t, recorder.Header().Get("Retry-After"), "1")
	var envelope struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Code    string  `json:"code"`
			Param   *string `json:"param"`
		} `json:"error"`
	}
	err := json.Unmarshal(recorder.Body.Bytes(), &envelope)
	testutil.CheckNoError(t, err)
	if envelope.Error.Message != "server is overloaded; retry later" ||
		envelope.Error.Type != "server_error" ||
		envelope.Error.Code != "server_overloaded" || envelope.Error.Param != nil {
		t.Fatalf("unexpected OpenAI error envelope: %+v", envelope.Error)
	}
	testutil.Equal(t, cl.rejectedReqs.Load(), 1)

	close(release)
	<-done
}

func TestLimitPreservesExecutionTimeout(t *testing.T) {
	cl := NewConcurrencyLimiter(1, 20*time.Millisecond)
	contextErr := make(chan error, 1)
	h := cl.Limit(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		contextErr <- r.Context().Err()
		w.WriteHeader(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	h(recorder, httptest.NewRequest(http.MethodGet, "http://x/", nil))
	got := <-contextErr
	testutil.False(t, got == nil, "ordinary limiter did not apply execution timeout")
	testutil.Equal(t, recorder.Code, http.StatusNoContent)
}
