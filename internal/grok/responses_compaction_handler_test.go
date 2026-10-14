package grok

import (
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/responses"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/testutil"
)

// TestHandleResponsesCompactSealsGatewaySummary is the end-to-end proof: the
// gateway runs the summary turn itself, seals it, and returns a portable
// compaction item instead of an upstream blob only one account can read.
func TestHandleResponsesCompactSealsGatewaySummary(t *testing.T) {
	var received map[string]any
	upstream := compactionUpstream(t, compactionTestSummary, &received)
	defer upstream.Close()
	codecCipher := testCompactionCipher(t)
	h, s, cleanup := setupCompactionHandler(t, upstream)
	defer cleanup()

	body := `{"model":"grok-4.5","input":[{"type":"message","role":"user","content":"hello"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleResponsesCompact(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	got := rec.Header().Get("Content-Type")
	testutil.Falsef(t, !strings.Contains(got, "application/json"), "Content-Type=%q", got)
	_ = s

	// The sample the gateway sent upstream is the canonical Build compaction turn.
	testutil.False(t, received == nil, "upstream saw no request")
	testutil.Equal(t, received["stream"], true)
	testutil.Equal(t, received["store"], false)
	if received["instructions"] != nil {
		t.Fatalf("sample instructions=%v", received["instructions"])
	}
	items := received["input"].([]any)
	last := items[len(items)-1].(map[string]any)
	testutil.EqualAny(t, last["content"], responses.CompactionPrompt)

	var payload map[string]any
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload), "decode response: %v")
	output := payload["output"].([]any)
	item := output[0].(map[string]any)
	testutil.Equal(t, item["type"], "compaction")
	blob := item["encrypted_content"].(string)
	testutil.Falsef(t, !strings.HasPrefix(blob, responses.CompactionPrefix), "blob=%q is not gateway-owned", blob)
	codec := responses.NewCompactionCodec(codecCipher)
	summary, owned, _, err := codec.Decode("", blob)
	testutil.Falsef(t, err != nil || !owned, "decode blob owned=%v err=%v", owned, err)
	testutil.MustContain(t, summary, "This session is being continued from a previous conversation")
	testutil.MustContain(t, summary, "Primary Request and Intent")
}

// TestHandleResponsesExpandsGatewayCompactionHistory proves the other half: the
// sealed summary comes back as an ordinary user message, so any account can
// serve the continuation.
func TestHandleResponsesExpandsGatewayCompactionHistory(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		testutil.NoError(t, json.Unmarshal(body, &decoded), "decode upstream body: %v")
		received = decoded
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()

	h, _, cleanup := setupCompactionHandler(t, upstream)
	defer cleanup()

	codec := responses.NewCompactionCodec(testCompactionCipher(t))
	blob, err := codec.Encode("session-a", "Summary:\ncarried forward")
	testutil.NoError(t, err, "encode: %v")
	body, _ := json.Marshal(map[string]any{
		"model": "grok-4.5", "stream": true,
		"input": []any{
			map[string]any{"type": "compaction", "encrypted_content": blob},
			map[string]any{"type": "message", "role": "user", "content": "next question"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.HandleResponses(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.False(t, received == nil, "upstream saw no request")
	items := received["input"].([]any)
	for index, raw := range items {
		item, ok := raw.(map[string]any)
		testutil.Falsef(t, ok && item["type"] == "compaction", "input[%d] still carries a compaction item: %#v", index, item)
	}
	first := items[0].(map[string]any)
	testutil.Equal(t, first["type"], "message")
	testutil.Equal(t, first["role"], "user")
	parts := first["content"].([]any)
	testutil.Equal(t, parts[0].(map[string]any)["text"], "Summary:\ncarried forward")
}

// A blob this gateway cannot open is a 400 that names the offending item. It must
// never be dropped silently or forwarded as an opaque blob.
func TestHandleResponsesRejectsUnreadableGatewayCompactionBlob(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	h, _, cleanup := setupCompactionHandler(t, upstream)
	defer cleanup()

	body, _ := json.Marshal(map[string]any{
		"model": "grok-4.5", "stream": false,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hi"},
			map[string]any{"type": "compaction", "encrypted_content": responses.CompactionPrefix + "broken"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.HandleResponses(rec, req)

	testutil.Equal(t, rec.Code, http.StatusBadRequest)
	var payload map[string]any
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload), "decode error body: %v")
	errObj := payload["error"].(map[string]any)
	testutil.Equal(t, errObj["code"], "invalid_compaction_blob")
	testutil.Equal(t, errObj["param"], "input[1].encrypted_content")
	testutil.Equal(t, upstreamCalls, 0)
}

// Without a sealing key the feature is off, and a compaction trigger keeps the
// old behaviour: it is relayed to the upstream rather than answered locally.
func TestHandleResponsesRelaysCompactionTriggerWhenDisabled(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		received = decoded
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_3\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()

	h, _, cleanup := setupCompactionHandler(t, upstream)
	defer cleanup()
	h.SetCompactionCipher(nil)
	testutil.False(t, h.GatewayCompactionEnabled(), "compaction reported as enabled without a cipher")

	body, _ := json.Marshal(map[string]any{
		"model": "grok-4.5", "stream": true,
		"input": []any{map[string]any{"type": "compaction_trigger"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.HandleResponses(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.False(t, received == nil, "the trigger was not relayed upstream")
	items := received["input"].([]any)
	testutil.Equal(t, items[0].(map[string]any)["type"], "compaction_trigger")
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		testutil.False(t, ok && item["content"] == responses.CompactionPrompt, "the gateway ran a summary turn while the feature was disabled")
	}
}

// A summary the model refuses to produce is not charged to the client as an
// answer: the gateway reports a 502 compaction_failed instead of an empty blob.
func TestHandleResponsesCompactFailsOnDegenerateSummary(t *testing.T) {
	attempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_deg\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"too short\"}]}]}}\n\n")
	}))
	defer upstream.Close()
	h, _, cleanup := setupCompactionHandler(t, upstream)
	defer cleanup()

	body := `{"model":"grok-4.5","input":[{"type":"message","role":"user","content":"hello"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	rec := httptest.NewRecorder()
	// A degenerate summary is retryable, so this exercises the real retry loop;
	// only the pause is shrunk, because the production value is three seconds.
	previousPause := responses.SetCompactionRetryPause(time.Millisecond)
	defer responses.SetCompactionRetryPause(previousPause)
	h.HandleResponsesCompact(rec, req)

	testutil.Equal(t, rec.Code, http.StatusBadGateway)
	var payload map[string]any
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload), "decode error body: %v")
	errObj, _ := payload["error"].(map[string]any)
	testutil.Equal(t, errObj["code"], "compaction_failed")
	testutil.Equal(t, attempts, responses.CompactionMaxAttempts)
}
