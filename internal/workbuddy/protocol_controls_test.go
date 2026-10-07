package workbuddy

import (
	"encoding/json"
	"orchids-api/internal/upstream"
	"testing"
)

func TestProtocolControlsInWireBody(t *testing.T) {
	zero, limit, parallel := 0.0, 17, false
	req := upstream.UpstreamRequest{Model: "m", MaxTokens: &limit, Temperature: &zero, ParallelToolCalls: &parallel, PromptCacheKey: "cache", ResponseText: map[string]interface{}{"format": map[string]interface{}{"type": "json_schema", "name": "answer", "schema": map[string]interface{}{"type": "object"}, "strict": true}}}
	raw, err := (&Client{}).buildBody(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	schema := body["response_format"].(map[string]interface{})["json_schema"].(map[string]interface{})
	if schema["name"] != "answer" || schema["strict"] != true || body["max_tokens"] != float64(17) || body["temperature"] != float64(0) {
		t.Fatalf("controls lost: %s", raw)
	}
	for _, key := range []string{"parallel_tool_calls", "prompt_cache_key"} {
		if _, exists := body[key]; exists {
			t.Fatalf("unsupported compatibility control %s leaked: %s", key, raw)
		}
	}
	req.Include = []string{"reasoning.encrypted_content"}
	if _, err := (&Client{}).buildBody(req); err == nil {
		t.Fatal("unsupported include accepted")
	}
}

func TestDefaultOutputLimit(t *testing.T) {
	raw, err := (&Client{}).buildBody(upstream.UpstreamRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["max_tokens"] != float64(8192) {
		t.Fatalf("default limit missing: %s", raw)
	}
}

func TestConfiguredOutputBudgetPreservesExplicitLimit(t *testing.T) {
	c := &Client{defaultMaxTokens: 16384}
	for _, limit := range []*int{nil, new(int)} {
		if limit != nil {
			*limit = 64
		}
		raw, err := c.buildBody(upstream.UpstreamRequest{Model: "m", MaxTokens: limit})
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		want := 16384
		if limit != nil {
			want = 64
		}
		if body["max_tokens"] != float64(want) {
			t.Fatalf("budget changed: %s", raw)
		}
	}
}
