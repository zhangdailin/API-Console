package cline

import (
	"encoding/json"
	"orchids-api/internal/upstream"
	"testing"
)

func TestProtocolControlsInWireBody(t *testing.T) {
	zero, limit, parallel := 0.0, 17, false
	req := upstream.UpstreamRequest{Model: "m", MaxTokens: &limit, Temperature: &zero, ParallelToolCalls: &parallel, PromptCacheKey: "cache", ResponseFormat: map[string]any{"type": "json_object"}}
	raw, err := buildChatBody(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["response_format"].(map[string]any)["type"] != "json_object" || body["prompt_cache_key"] != "cache" || body["max_tokens"] != float64(17) || body["temperature"] != float64(0) || body["parallel_tool_calls"] != false {
		t.Fatalf("controls lost: %s", raw)
	}
	req.ResponsesTools = []map[string]any{{"type": "web_search"}}
	if _, err := buildChatBody(req, "m"); err == nil {
		t.Fatal("unsupported hosted tools accepted")
	}
}
