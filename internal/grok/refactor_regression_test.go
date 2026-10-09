package grok

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/responses"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
	"time"
)

func TestRefactorNativeUsageSurvivesFullCaptureOverflow(t *testing.T) {
	frame := parityFrame("response.in_progress", map[string]interface{}{"padding": strings.Repeat("a", 128<<10)})
	stream := strings.Repeat(frame, 70) + parityFrame("response.completed", map[string]interface{}{"response": map[string]interface{}{
		"id": "resp_a", "status": "completed", "usage": map[string]interface{}{"input_tokens": 100, "output_tokens": 10},
	}})
	id, captured, result := copyNativeCLIResponseAndCaptureModel(httptest.NewRecorder(), strings.NewReader(stream), "text/event-stream", "grok-4.6")
	testutil.Fail(t, id != "resp_a" || len(captured) != responses.MaxEventBytes || result.Err != nil || interfaceToInt(result.Usage["completion_tokens"]) != 10, id, len(captured), result)
}

type refactorErrorReader struct{ err error }

func (r refactorErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestRefactorNativeJSONReadErrorIsNotAuditedAsSuccess(t *testing.T) {
	readErr := errors.New("synthetic transport failure")
	source := io.MultiReader(strings.NewReader("{\"id\":\"resp_a\",\"status\":\"completed\"}"), refactorErrorReader{readErr})
	_, _, result := copyNativeCLIResponseAndCaptureModel(httptest.NewRecorder(), source, "application/json", "grok-4.6")
	testutil.Fail(t, !errors.Is(result.Err, readErr) || result.Finish != "error", result)
}

func TestRefactorChatBridgePropagatesErrorAndClosesPipe(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{"))
	var saved io.Reader
	(&Handler{}).withChatStream(req, func(status int, header http.Header, reader io.Reader) {
		saved = reader
		body, err := io.ReadAll(reader)
		testutil.Fail(t, err != nil || status != http.StatusBadRequest || !strings.Contains(string(body), "invalid json"), status, err, string(body))
	})
	testutil.False(t, saved == nil, "bridge did not forward response")
	if _, err := saved.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("bridge left reader open", err)
	}
}

func TestRefactorChatBridgeCancellationDoesNotWaitForHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{")).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Handler{}).withChatStream(req, func(_ int, _ http.Header, reader io.Reader) { _, _ = io.Copy(io.Discard, reader) })
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled bridge stayed blocked")
	}
}
