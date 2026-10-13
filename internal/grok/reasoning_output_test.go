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
	"orchids-api/internal/responses"
	"orchids-api/internal/testutil"
)

func reasoningOutputResponse() map[string]interface{} {
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

func reasoningOutputStream() string {
	response := reasoningOutputResponse()
	reason := response["output"].([]interface{})[0]
	return parityFrame("response.output_item.added", map[string]interface{}{"output_index": 0, "item": map[string]interface{}{"id": "reason-1", "type": "reasoning"}}) +
		parityFrame("response.reasoning_text.delta", map[string]interface{}{"item_id": "reason-1", "delta": "PRIVATE_RAW"}) +
		parityFrame("response.reasoning_summary_text.delta", map[string]interface{}{"item_id": "reason-1", "delta": "PRIVATE_SUMMARY_DELTA"}) +
		parityFrame("response.output_item.done", map[string]interface{}{"output_index": 0, "item": reason}) +
		parityFrame("response.output_text.delta", map[string]interface{}{"item_id": "answer-1", "output_index": 1, "content_index": 0, "delta": "Public answer"}) +
		parityFrame("response.completed", map[string]interface{}{"response": response})
}

func TestNativeReasoningOutputKeepsReplayAndAccounting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			raw, _ := json.Marshal(reasoningOutputResponse())
			body, contentType := string(raw), "application/json"
			if stream {
				body, contentType = reasoningOutputStream(), "text/event-stream"
			}
			w := httptest.NewRecorder()
			id, captured, result := copyNativeCLIResponseAndCaptureModel(w, iotest.OneByteReader(strings.NewReader(body)), contentType, "grok-4.6")
			if result.Err != nil || result.Finish != "stop" || id != "resp-visible" {
				t.Fatalf("unexpected completion: id=%s result=%+v", id, result)
			}
			output := w.Body.String()
			for _, preserved := range []string{"PRIVATE_RAW", "PRIVATE_SUMMARY", "Public answer", "opaque-replay", "call-1", "tool parameter", "reasoning_tokens"} {
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

func TestChatAndMessagesKeepReasoningSeparateFromAnswer(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			w := httptest.NewRecorder()
			h := &Handler{}
			req := &chatwire.Request{Model: "grok-4.6", Stream: stream}
			var result chatOutcome
			if stream {
				result = h.streamBuildChatHolding(w, req, strings.NewReader(reasoningOutputStream()), nil)
			} else {
				raw, _ := json.Marshal(reasoningOutputResponse())
				result = h.collectBuildChat(w, req, bytes.NewReader(raw))
			}
			if result.Err != nil || !result.Quality.SawReasoning || result.Quality.ReasoningChars == 0 {
				t.Fatalf("lost internal reasoning evidence: %+v", result)
			}
			var answer, thinking strings.Builder
			checkChat := func(message map[string]interface{}) {
				answer.WriteString(responses.StreamString(message["content"]))
				thinking.WriteString(responses.StreamString(message["reasoning_content"]))
			}
			var messageAnswer, messageThinking strings.Builder
			if stream {
				testutil.NoError(t, responses.ReadSSE(bytes.NewReader(w.Body.Bytes()), func(_ string, data string) error {
					if data == "[DONE]" {
						return nil
					}
					var chunk map[string]interface{}
					if err := json.Unmarshal([]byte(data), &chunk); err != nil {
						return err
					}
					for _, value := range responses.InterfaceSlice(chunk["choices"]) {
						choice := value.(map[string]interface{})
						checkChat(choice["delta"].(map[string]interface{}))
					}
					return nil
				}))
				var out bytes.Buffer
				testutil.NoError(t, translateOpenAIChatStreamToAnthropicWithInput(&out, bytes.NewReader(w.Body.Bytes()), req.Model, 10))
				blocks := map[int]string{}
				testutil.NoError(t, responses.ReadSSE(bytes.NewReader(out.Bytes()), func(event, data string) error {
					var frame map[string]interface{}
					if err := json.Unmarshal([]byte(data), &frame); err != nil {
						return err
					}
					index := responses.InterfaceToInt(frame["index"])
					if event == "content_block_start" {
						block := frame["content_block"].(map[string]interface{})
						blocks[index] = responses.StreamString(block["type"])
					}
					if event == "content_block_delta" {
						delta := frame["delta"].(map[string]interface{})
						switch delta["type"] {
						case "text_delta":
							testutil.Equal(t, blocks[index], "text")
							messageAnswer.WriteString(responses.StreamString(delta["text"]))
						case "thinking_delta":
							testutil.Equal(t, blocks[index], "thinking")
							messageThinking.WriteString(responses.StreamString(delta["thinking"]))
						}
					}
					return nil
				}))
			} else {
				var chat map[string]interface{}
				testutil.NoError(t, json.Unmarshal(w.Body.Bytes(), &chat))
				checkChat(responses.InterfaceSlice(chat["choices"])[0].(map[string]interface{})["message"].(map[string]interface{}))
				message := anthropicResponseFromChat(req.Model, chat)
				for _, value := range responses.InterfaceSlice(message["content"]) {
					block := value.(map[string]interface{})
					switch block["type"] {
					case "text":
						messageAnswer.WriteString(responses.StreamString(block["text"]))
					case "thinking":
						messageThinking.WriteString(responses.StreamString(block["thinking"]))
					}
				}
			}
			testutil.Equal(t, answer.String(), "Public answer")
			testutil.Equal(t, thinking.String(), "PRIVATE_RAW")
			testutil.Equal(t, messageAnswer.String(), "Public answer")
			testutil.Equal(t, messageThinking.String(), "PRIVATE_RAW")
		})
	}
}

func TestNativeResourceKeepsReasoning(t *testing.T) {
	raw, _ := json.Marshal(reasoningOutputResponse())
	response := &http.Response{Body: io.NopCloser(bytes.NewReader(raw))}
	if err := restoreNativeResourceNamespaces(response, nil); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(response.Body)
	testutil.NoError(t, err)
	testutil.Equal(t, string(output), string(raw))
}

func TestNativeReasoningOutputReportsShortWrites(t *testing.T) {
	for _, stream := range []bool{false, true} {
		raw, _ := json.Marshal(reasoningOutputResponse())
		body, contentType := string(raw), "application/json"
		if stream {
			body, contentType = reasoningOutputStream(), "text/event-stream"
		}
		_, _, result := copyNativeCLIResponseAndCaptureModel(compatShortWriter{httptest.NewRecorder()}, strings.NewReader(body), contentType, "grok-4.6")
		if !errors.Is(result.Err, io.ErrShortWrite) || result.Finish != "error" {
			t.Fatalf("short write reported as success: %+v", result)
		}
	}
}
