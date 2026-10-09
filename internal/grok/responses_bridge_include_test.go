package grok

import (
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/responses"
	"strings"
	"sync"
	"testing"
)

func TestBridgeConsumesEncryptedReasoningProjection(t *testing.T) {
	for _, channel := range []string{"qoder", "workbuddy", "cline"} {
		for _, stream := range []string{"true", "false"} {
			var mu sync.Mutex
			var calls []recordedChatCall
			bridge := ResponsesBridgeHandler(recordingChat(t, &calls, &mu), responses.BridgeOptions{})
			rec := httptest.NewRecorder()
			bridge(rec, httptest.NewRequest(http.MethodPost, "/"+channel+"/v1/responses", strings.NewReader(`{"model":"test","input":"hello","stream":`+stream+`,"include":["reasoning.encrypted_content"]}`)))
			if rec.Code != 200 || len(calls) != 1 {
				t.Fatalf("%s: %d %s", channel, rec.Code, rec.Body.String())
			}
			if value, exists := calls[0].body["include"]; exists && value != nil {
				if items, ok := value.([]interface{}); !ok || len(items) != 0 {
					t.Fatalf("include leaked: %v", value)
				}
			}
			if rec.Header().Get("X-Grok2API-Compatibility-Warnings") == "" || strings.Contains(rec.Body.String(), `"encrypted_content"`) {
				t.Fatal("missing warning or fabricated encrypted output")
			}
		}
	}
}

func TestBridgeStillRejectsEncryptedInput(t *testing.T) {
	called := false
	bridge := ResponsesBridgeHandler(func(http.ResponseWriter, *http.Request) { called = true }, responses.BridgeOptions{})
	rec := httptest.NewRecorder()
	bridge(rec, httptest.NewRequest("POST", "/qoder/v1/responses", strings.NewReader(`{"model":"test","input":[{"type":"reasoning","encrypted_content":"opaque"}],"include":["reasoning.encrypted_content"]}`)))
	if called || rec.Code == 200 {
		t.Fatalf("encrypted input accepted: %d", rec.Code)
	}
}

func TestBridgeDisablesHostedSearchWithoutInventingFunction(t *testing.T) {
	for _, channel := range []string{"qoder", "workbuddy", "cline"} {
		var mu sync.Mutex
		var calls []recordedChatCall
		bridge := ResponsesBridgeHandler(recordingChat(t, &calls, &mu), responses.BridgeOptions{})
		rec := httptest.NewRecorder()
		bridge(rec, httptest.NewRequest("POST", "/"+channel+"/v1/responses", strings.NewReader(`{"model":"test","input":"hello","include":["reasoning.encrypted_content"],"tools":[{"type":"web_search","external_web_access":false},{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`)))
		if rec.Code != 200 || len(calls) != 1 {
			t.Fatalf("%s: %d %s", channel, rec.Code, rec.Body.String())
		}
		tools, _ := calls[0].body["tools"].([]interface{})
		if len(tools) != 1 || strings.Contains(rec.Body.String(), "web_search_call") {
			t.Fatal("invented search or lost real tool")
		}
		if !strings.Contains(strings.Join(rec.Header().Values("X-Grok2API-Compatibility-Warnings"), ";"), "web search disabled") {
			t.Fatal("missing search warning")
		}
	}
}
