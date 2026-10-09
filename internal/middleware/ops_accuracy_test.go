package middleware

import (
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
	"time"
)

func TestInferenceOutcomeExcludesNonGeneration(t *testing.T) {
	outcomes := captureOutcomes(t)
	h := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	for _, tc := range []struct{ method, path, channel string }{
		{"GET", "/v1/models", "http"}, {"POST", "/v1/admin/verify", "http"},
		{"POST", "/cline/v1/messages/count_tokens", "http"}, {"GET", "/v1/videos/job", "http"},
		{"GET", "/v1/videos", "http"}, {"OPTIONS", "/v1/responses", "http"},
		{"POST", "/cline/v1/nonexistent", "http"}, {"POST", "/v1/files", "http"},
		{"POST", "/cline/v1/messages", "cline"}, {"POST", "/qoder/v1/chat/completions", "qoder"},
		{"POST", "/workbuddy/v1/messages", "workbuddy"}, {"POST", "/grok/v1/messages", "grok"},
		{"POST", "/v1/responses", "http"}, {"POST", "/v1/images/generations", "http"},
		{"POST", "/v1/videos", "http"}, {"POST", "/v1/audio/transcriptions", "http"},
		{"GET", "/v1/stt", "http"}, {"GET", "/grok/v1/realtime", "http"},
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, nil))
		testutil.CheckEqual(t, (*outcomes)[len(*outcomes)-1].Channel, tc.channel)
	}
}

func TestTTFTWaitsForGeneratedContent(t *testing.T) {
	for _, tc := range []struct{ name, control, generated string }{
		{"messages", "event: message_start\ndata: {\"type\":\"message_start\"}\n\n", "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n"},
		{"responses", "data: {\"type\":\"response.created\"}\n\n", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"},
		{"chat", "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcomes := captureOutcomes(t)
			h := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				w.WriteHeader(200)
				w.Write([]byte(": ping\n\n" + tc.control))
				time.Sleep(30 * time.Millisecond)
				// Writes may split anywhere, including inside an event field or JSON value.
				for _, b := range []byte(tc.generated) {
					w.Write([]byte{b})
				}
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/grok/v1/responses", nil))
			got := (*outcomes)[0]
			testutil.Falsef(t, got.FirstTokenMS < 25 || got.FirstTokenMS > got.DurationMS, "incorrect TTFT: %+v", got)
			testutil.Equal(t, rec.Body.String(), ": ping\n\n"+tc.control+tc.generated)
		})
	}
}

func TestTTFTNoGeneratedOutputHasNoSample(t *testing.T) {
	outcomes := captureOutcomes(t)
	h := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(200)
		w.Write([]byte("data: {\"type\":\"response.created\"}\n\ndata: {\"error\":{\"message\":\"failed\"}}\n\ndata: [DONE]\n\n"))
		MarkStreamFailure(w)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/grok/v1/responses", nil))
	if got := (*outcomes)[0]; got.FirstTokenMS != 0 || got.StatusClass != "stream_error" {
		t.Fatal(got)
	}
}

func TestTokenDetectorFramingAndContent(t *testing.T) {
	for _, data := range []string{
		`{"choices":[{"delta":{"reasoning_content":"think"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}`,
		`{"type":"content_block_delta","delta":{"thinking":"think"}}`,
		`{"type":"content_block_delta","delta":{"partial_json":"{}"}}`,
		`{"type":"response.function_call_arguments.delta","delta":"{}"}`,
	} {
		var d tokenSSEDetector
		testutil.CheckFalsef(t, !d.observe([]byte(": keepalive\r\nevent: delta\r\ndata: "+data+"\r\n\r\n")), "missing content: %s", data)
	}
	var d tokenSSEDetector
	testutil.False(t, d.observe([]byte("data: "+strings.Repeat("x", maxTokenEventBytes+10)+"\nignored line\n\n")), "oversized event accepted")
	testutil.False(t, !d.observe([]byte("event: response.output_text.delta\ndata: {\"delta\":\"hi\"}\n\n")), "did not recover after oversized event")
}
