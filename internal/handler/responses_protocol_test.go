package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
)

func TestResponsesControlsReachRealSharedHandler(t *testing.T) {
	for _, channel := range []string{"workbuddy", "cline"} {
		t.Run(channel, func(t *testing.T) {
			h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
			// The request is strict, so the recorded answer has to satisfy its
			// schema; {} is the empty object that schema describes.
			client := &relayRecordingClient{answer: "{}"}
			h.client = client
			body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":17,"temperature":0,"parallel_tool_calls":false,"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"},"strict":true}}}`
			w := httptest.NewRecorder()
			h.HandleMessages(w, httptest.NewRequest(http.MethodPost, "/"+channel+"/v1/chat/completions", strings.NewReader(body)))
			if w.Code != 200 || len(client.requests) != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, len(client.requests), w.Body.String())
			}
			got := client.requests[0]
			if got.ResponseText["format"] == nil || got.MaxTokens == nil || *got.MaxTokens != 17 || got.Temperature == nil || *got.Temperature != 0 || got.ParallelToolCalls == nil || *got.ParallelToolCalls {
				t.Fatalf("controls lost: %#v", got)
			}
		})
	}
}

func TestUnsupportedResponsesControlsFailBeforeUpstream(t *testing.T) {
	for _, extra := range []string{`"include":["reasoning.encrypted_content"]`, `"x_responses_tools":[{"type":"web_search"}]`, `"text":{"verbosity":"high"}`, `"mcp_servers":[{"name":"s","url":"https://example.test"}]`} {
		h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
		client := &relayRecordingClient{}
		h.client = client
		w := httptest.NewRecorder()
		h.HandleMessages(w, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],`+extra+`}`)))
		if w.Code != 400 || len(client.requests) != 0 {
			t.Fatalf("extra=%s status=%d calls=%d body=%s", extra, w.Code, len(client.requests), w.Body.String())
		}
	}
}
