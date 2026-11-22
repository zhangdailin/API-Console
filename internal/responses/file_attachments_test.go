package responses

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/store"
)

func TestResponsesBridgeRejectsInvalidFileSourceBeforeChat(t *testing.T) {
	bridge := ResponsesBridgeHandler(func(http.ResponseWriter, *http.Request) {
		t.Fatal("an invalid file reached Chat")
	}, BridgeOptions{})
	for _, part := range []string{
		`{"type":"input_file","filename":"report.pdf"}`,
		`{"type":"input_file","file_url":"https://example.com/report.pdf","file_data":"data:application/pdf;base64,AAAA"}`,
		`{"type":"input_file","file_url":"https://example.com/report.pdf","file":{"file_id":"file_old"}}`,
	} {
		rec := httptest.NewRecorder()
		body := `{"model":"grok-chat-auto","input":[{"role":"user","content":[{"type":"input_text","text":"Summarize"},` + part + `]}]}`
		bridge(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/responses", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "input[0]: content[1]") {
			t.Fatalf("malformed attachment returned HTTP %d: %s", rec.Code, rec.Body)
		}
	}
}

func TestResponsesBridgeReplaysStoredFileOnLaterTurns(t *testing.T) {
	const fileURL = "https://example.com/report.pdf"
	turn := 0
	bridge := ResponsesBridgeHandler(func(w http.ResponseWriter, r *http.Request) {
		turn++
		body, err := io.ReadAll(r.Body)
		if err != nil || strings.Count(string(body), fileURL) != 1 {
			t.Fatalf("turn %d did not forward the original file exactly once: %s (%v)", turn, body, err)
		}
		if turn == 3 && !strings.Contains(string(body), "Second question") {
			t.Fatal("the third turn lost the second user message")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"Reply"},"finish_reason":"stop"}]}`)
	}, BridgeOptions{Store: store.NewMemoryResponseStore(time.Hour), StoredInputReplayLimit: 4096})
	call := func(body string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		bridge(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/responses", strings.NewReader(body)))
		var result struct{ ID string }
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK || result.ID == "" {
			t.Fatalf("continuation returned HTTP %d: %s (%v)", rec.Code, rec.Body, err)
		}
		return result.ID
	}
	first := call(`{"model":"grok-chat-auto","store":true,"input":[{"role":"user","content":[{"type":"input_file","file_url":"` + fileURL + `"}]}]}`)
	second := call(`{"model":"grok-chat-auto","store":true,"previous_response_id":"` + first + `","input":"Second question"}`)
	call(`{"model":"grok-chat-auto","store":false,"previous_response_id":"` + second + `","input":"Third question"}`)
}
