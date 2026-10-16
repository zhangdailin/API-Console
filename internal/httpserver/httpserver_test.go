package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/middleware"
	"orchids-api/internal/testutil"
)

type downstreamFailureWriter struct {
	*httptest.ResponseRecorder
	flushErr  error
	deadlines int
	writes    int
}

func (w *downstreamFailureWriter) SetWriteDeadline(time.Time) error {
	w.deadlines++
	return nil
}
func (w *downstreamFailureWriter) FlushError() error { return w.flushErr }
func (w *downstreamFailureWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.ResponseRecorder.Write(p)
}

func TestCheckedStreamFlushFailureStopsDeliveryAndMarksTrace(t *testing.T) {
	failure := errors.New("client disappeared on flush")
	target := &downstreamFailureWriter{ResponseRecorder: httptest.NewRecorder(), flushErr: failure}
	trace := middleware.NewTracedResponseWriter(target)
	w := &CheckedStreamWriter{Target: DeadlineResponseWriter{ResponseWriter: trace}}
	if err := w.WriteSSE("demo", []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	if !errors.Is(w.Err, failure) || !trace.StreamFailed() || target.deadlines != 2 {
		t.Fatalf("err=%v marked=%v deadlines=%d", w.Err, trace.StreamFailed(), target.deadlines)
	}
	if err := w.WriteSSE("demo", []byte(`{"late":true}`)); !errors.Is(err, failure) || target.writes != 1 {
		t.Fatalf("late delivery continued: err=%v writes=%d", err, target.writes)
	}
}

func TestDecodeJSONBodyRequiresOneCompleteValue(t *testing.T) {
	for _, body := range []string{`{"a":1} {"b":2}`, `{"a":1} null`, `{"a":1} trailing`, ""} {
		t.Run(body, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/", strings.NewReader(body))
			var value map[string]any
			if DecodeJSONBody(rec, req, &value) || rec.Code != 400 {
				t.Fatalf("invalid body accepted: %d", rec.Code)
			}
		})
	}
	var value map[string]any
	rec := httptest.NewRecorder()
	if !DecodeJSONBody(rec, httptest.NewRequest("POST", "/", strings.NewReader("{\"a\":1} \r\n\t")), &value) {
		t.Fatal("trailing whitespace rejected")
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.Body = nil
	if DecodeJSONBody(httptest.NewRecorder(), req, &value) {
		t.Fatal("nil body accepted")
	}
}

func TestDecodeJSONBodyEnforcesLimitAfterValidPrefix(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"+strings.Repeat(" ", 1024)))
	var value map[string]any
	if DecodeJSONBodyLimit(rec, req, &value, 1024) || rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized suffix accepted: %d", rec.Code)
	}
}

func TestStreamThroughChatCancellationInterruptsIdleConsumer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	reading, done, producerDone := make(chan struct{}), make(chan error, 1), make(chan struct{})
	go func() {
		StreamThroughChat(req, func(w http.ResponseWriter, r *http.Request) {
			defer close(producerDone)
			w.WriteHeader(http.StatusOK)
			<-r.Context().Done()
		}, func(_ int, _ http.Header, reader io.Reader) {
			close(reading)
			_, err := io.ReadAll(reader)
			done <- err
		})
	}()
	select {
	case <-reading:
	case <-time.After(time.Second):
		t.Fatal("consumer did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("consumer remained blocked on cancellation")
	}
	select {
	case <-producerDone:
	case <-time.After(time.Second):
		t.Fatal("producer was not canceled")
	}
}

type shortWriter struct{ http.ResponseWriter }

func (w shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestWriteSSEBytesWritesEventFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteSSEBytes(rec, "demo", []byte(`{"ok":true}`))

	got := rec.Body.String()
	testutil.MustContainAll(t, got, "event: demo\n", `data: {"ok":true}`)
}

func TestWriteSSEBytesPropagatesShortWrite(t *testing.T) {
	writer := shortWriter{httptest.NewRecorder()}
	err := WriteSSEBytes(writer, "demo", []byte(`{"ok":true}`))
	testutil.Falsef(t, !errors.Is(err, io.ErrShortWrite), "error=%v", err)
}

func TestStreamResponseHeadersMatchSSEProxyContract(t *testing.T) {
	recorder := httptest.NewRecorder()
	recorder.Header().Set("Connection", "keep-alive")
	StreamResponseHeaders(recorder)
	got := recorder.Header().Get("Content-Type")
	testutil.Falsef(t, got != "text/event-stream; charset=utf-8", "content-type=%q", got)
	testutil.Falsef(t, recorder.Header().Get("X-Accel-Buffering") != "no" || recorder.Header().Get("Connection") != "", "headers=%v", recorder.Header())
}

func TestErrorCodeForStatusCoversClientAndServerStatuses(t *testing.T) {
	cases := map[int]string{
		http.StatusBadRequest:            "invalid_request",
		http.StatusUnauthorized:          "invalid_api_key",
		http.StatusForbidden:             "permission_denied",
		http.StatusNotFound:              "not_found",
		http.StatusMethodNotAllowed:      "method_not_allowed",
		http.StatusConflict:              "conflict",
		http.StatusRequestTimeout:        "timeout",
		http.StatusRequestEntityTooLarge: "request_too_large",
		http.StatusUnsupportedMediaType:  "unsupported_media_type",
		http.StatusTooManyRequests:       "rate_limit_exceeded",
		http.StatusServiceUnavailable:    "service_unavailable",
		http.StatusGatewayTimeout:        "timeout",
		http.StatusInternalServerError:   "server_error",
		http.StatusTeapot:                "invalid_request",
	}
	for status, want := range cases {
		testutil.Equal(t, ErrorCodeForStatus(status), want)
	}
}

// WriteErrorCode raises a sub-400 status to 502: a caller that reached the
// error writer has failed, and a 200 carrying an error body is invisible to
// every client that only looks at the status line.
func TestWriteErrorCodeRaisesNonErrorStatusToBadGateway(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteErrorCode(rec, http.StatusOK, "boom", "something failed")
	testutil.Equal(t, rec.Code, http.StatusBadGateway)
	testutil.MustContainAll(t, rec.Body.String(), `"code":"boom"`, `"message":"something failed"`, `"param":null`)
}

func TestWriteErrorDerivesCodeFromStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusNotFound, "missing")
	testutil.Equal(t, rec.Code, http.StatusNotFound)
	testutil.MustContainAll(t, rec.Body.String(), `"code":"not_found"`, `"type":"invalid_request_error"`)
}

func TestRequireMethodKeepsPlainTextVariantSeparate(t *testing.T) {
	envelope := httptest.NewRecorder()
	testutil.False(t, RequireMethod(envelope, httptest.NewRequest(http.MethodGet, "/x", nil), http.MethodPost))
	testutil.Equal(t, envelope.Code, http.StatusMethodNotAllowed)
	testutil.MustContain(t, envelope.Body.String(), `"error"`)

	plain := httptest.NewRecorder()
	testutil.False(t, RequirePlainMethod(plain, httptest.NewRequest(http.MethodGet, "/x", nil), http.MethodPost))
	testutil.Equal(t, plain.Code, http.StatusMethodNotAllowed)
	testutil.MustNotContain(t, plain.Body.String(), `"error"`)
	testutil.MustContain(t, plain.Body.String(), "Method not allowed")
}

func TestDecodeJSONBodyRejectsNonJSONContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"a":1}`))
	req.Header.Set("Content-Type", "text/plain")
	var payload map[string]any
	testutil.False(t, DecodeJSONBody(rec, req, &payload), "DecodeJSONBody accepted a non-JSON content type")
	testutil.Equal(t, rec.Code, http.StatusUnsupportedMediaType)
}

func BenchmarkWriteSSE_Bytes(b *testing.B) {
	writer := httptest.NewRecorder()
	data := []byte(`{"ok":true}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		writer.Body.Reset()
		WriteSSEBytes(writer, "demo", data)
	}
}

func TestJSONBodyAcceptsBeyondFormerLimit(t *testing.T) {
	body := "{}" + strings.Repeat(" ", 33<<20)
	var value map[string]any
	if !DecodeJSONBody(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader(body)), &value) {
		t.Fatal("large JSON rejected")
	}
	got, err := ReadBoundedJSONBody(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader(body)))
	if err != nil || string(got) != body {
		t.Fatal("large JSON changed", err)
	}
}
