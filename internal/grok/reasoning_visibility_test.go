package grok

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/config"
)

func visibilityResponse() map[string]interface{} {
	return map[string]interface{}{
		"id": "resp-visible", "object": "response", "status": "completed", "model": "grok-4.6",
		"output": []interface{}{
			map[string]interface{}{"id": "reason-1", "type": "reasoning", "encrypted_content": "opaque-replay",
				"content": []interface{}{map[string]interface{}{"type": "reasoning_text", "text": "PRIVATE_RAW"}},
				"summary": []interface{}{map[string]interface{}{"type": "summary_text", "text": "PRIVATE_SUMMARY"}}},
			map[string]interface{}{"id": "answer-1", "type": "message", "role": "assistant", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": "Public answer"}}},
			map[string]interface{}{"id": "tool-1", "type": "function_call", "call_id": "call-1", "name": "lookup", "arguments": `{"reasoning":"tool parameter"}`},
		},
		"usage": map[string]interface{}{"input_tokens": 10, "output_tokens": 20, "output_tokens_details": map[string]interface{}{"reasoning_tokens": 15}},
	}
}

func visibilityStream() string {
	response := visibilityResponse()
	reason := response["output"].([]interface{})[0]
	return parityFrame("response.output_item.added", map[string]interface{}{"output_index": 0, "item": map[string]interface{}{"id": "reason-1", "type": "reasoning"}}) +
		parityFrame("response.reasoning_text.delta", map[string]interface{}{"item_id": "reason-1", "delta": "PRIVATE_DELTA"}) +
		parityFrame("response.reasoning_summary_text.delta", map[string]interface{}{"item_id": "reason-1", "delta": "PRIVATE_SUMMARY_DELTA"}) +
		parityFrame("response.output_item.done", map[string]interface{}{"output_index": 0, "item": reason}) +
		parityFrame("response.output_text.delta", map[string]interface{}{"item_id": "answer-1", "output_index": 1, "content_index": 0, "delta": "Public answer"}) +
		parityFrame("response.completed", map[string]interface{}{"response": response})
}

func TestNativeReasoningVisibilityKeepsReplayAndAccounting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, hidden := range []bool{false, true} {
			t.Run(map[bool]string{false: "json", true: "sse"}[stream]+map[bool]string{false: "-visible", true: "-hidden"}[hidden], func(t *testing.T) {
				raw, _ := json.Marshal(visibilityResponse())
				body, contentType := string(raw), "application/json"
				if stream {
					body, contentType = visibilityStream(), "text/event-stream"
				}
				w := httptest.NewRecorder()
				id, captured, result := copyNativeResponseWithVisibility(w, iotest.OneByteReader(strings.NewReader(body)), contentType, "grok-4.6", hidden)
				if result.Err != nil || result.Finish != "stop" || id != "resp-visible" {
					t.Fatalf("unexpected completion: id=%s result=%+v", id, result)
				}
				output := w.Body.String()
				if strings.Contains(output, "PRIVATE_RAW") == hidden || strings.Contains(output, "PRIVATE_SUMMARY") == hidden {
					t.Fatalf("wrong reasoning visibility: %s", output)
				}
				if hidden && strings.Contains(output, "PRIVATE_DELTA") {
					t.Fatal("reasoning delta leaked")
				}
				for _, preserved := range []string{"Public answer", "opaque-replay", "call-1", "tool parameter", "reasoning_tokens"} {
					if !strings.Contains(output, preserved) {
						t.Fatalf("lost %s", preserved)
					}
				}
				if !strings.Contains(string(captured), "PRIVATE_RAW") || !strings.Contains(string(captured), "opaque-replay") {
					t.Fatal("private replay capture was filtered")
				}
				if result.Usage["completion_tokens_details"].(map[string]interface{})["reasoning_tokens"] != 15 {
					t.Fatalf("lost reasoning accounting: %+v", result.Usage)
				}
			})
		}
	}
}

func TestChatAndMessagesHideReasoningWithoutChangingQuality(t *testing.T) {
	for _, stream := range []bool{false, true} {
		w := httptest.NewRecorder()
		h := &Handler{cfg: &config.Config{GrokHideReasoning: true}}
		req := &chatwire.Request{Model: "grok-4.6", Stream: stream}
		var result chatOutcome
		if stream {
			result = h.streamBuildChatHolding(w, req, strings.NewReader(visibilityStream()), nil)
		} else {
			raw, _ := json.Marshal(visibilityResponse())
			result = h.collectBuildChat(w, req, bytes.NewReader(raw))
		}
		if result.Err != nil || !result.Quality.SawReasoning || result.Quality.ReasoningChars == 0 {
			t.Fatalf("lost internal reasoning evidence: %+v", result)
		}
		if strings.Contains(w.Body.String(), "reasoning_content") || strings.Contains(w.Body.String(), "PRIVATE_") || strings.Contains(w.Body.String(), "opaque-replay") {
			t.Fatalf("chat leaked thinking: %s", w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "Public answer") || !strings.Contains(w.Body.String(), "call-1") {
			t.Fatalf("chat lost answer/tool: %s", w.Body.String())
		}
		var messages []byte
		if stream {
			var out bytes.Buffer
			if err := translateOpenAIChatStreamToAnthropicWithInput(&out, bytes.NewReader(w.Body.Bytes()), req.Model, 10); err != nil {
				t.Fatal(err)
			}
			messages = out.Bytes()
		} else {
			var chat map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &chat); err != nil {
				t.Fatal(err)
			}
			messages, _ = json.Marshal(anthropicResponseFromChat(req.Model, chat))
		}
		if strings.Contains(string(messages), `"type":"thinking"`) || strings.Contains(string(messages), "PRIVATE_") {
			t.Fatalf("messages leaked thinking: %s", messages)
		}
	}
}

func TestNativeResourceReasoningVisibility(t *testing.T) {
	raw, _ := json.Marshal(visibilityResponse())
	response := &http.Response{Body: io.NopCloser(bytes.NewReader(raw))}
	if err := restoreNativeResourceNamespaces(response, nil, true); err != nil {
		t.Fatal(err)
	}
	filtered, _ := io.ReadAll(response.Body)
	if strings.Contains(string(filtered), "PRIVATE_") || !strings.Contains(string(filtered), "opaque-replay") || !strings.Contains(string(filtered), "Public answer") {
		t.Fatalf("wrong resource projection: %s", filtered)
	}
}

func TestNativeReasoningFilterDoesNotVisitToolOrMessageContent(t *testing.T) {
	envelope := map[string]interface{}{"output": []interface{}{
		map[string]interface{}{"type": "function_call", "arguments": `{"summary":"PRIVATE_RAW","content":"PRIVATE_SUMMARY"}`},
		map[string]interface{}{"type": "message", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": "PRIVATE_RAW"}}},
	}}
	before, _ := json.Marshal(envelope)
	if hideNativeReasoning(envelope) {
		t.Fatal("changed unrelated content")
	}
	after, _ := json.Marshal(envelope)
	if !bytes.Equal(before, after) {
		t.Fatal("rewrote tool arguments or answer")
	}
}

func TestHiddenNativeResponseReportsShortWrites(t *testing.T) {
	for _, stream := range []bool{false, true} {
		raw, _ := json.Marshal(visibilityResponse())
		body, contentType := string(raw), "application/json"
		if stream {
			body, contentType = visibilityStream(), "text/event-stream"
		}
		_, _, result := copyNativeResponseWithVisibility(compatShortWriter{httptest.NewRecorder()}, strings.NewReader(body), contentType, "grok-4.6", true)
		if !errors.Is(result.Err, io.ErrShortWrite) || result.Finish != "error" {
			t.Fatalf("short write reported as success: %+v", result)
		}
	}
}
