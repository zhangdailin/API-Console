package workbuddy

import (
	"encoding/json"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"testing"
)

func TestProtocolControlsInWireBody(t *testing.T) {
	zero, limit, parallel := 0.0, 17, false
	req := upstream.UpstreamRequest{Model: "m", MaxTokens: &limit, Temperature: &zero, ParallelToolCalls: &parallel, PromptCacheKey: "cache", ResponseText: map[string]any{"format": map[string]any{"type": "json_schema", "name": "answer", "schema": map[string]any{"type": "object"}, "strict": true}}}
	raw, err := (&Client{}).buildBody(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	schema := body["response_format"].(map[string]any)["json_schema"].(map[string]any)
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

func TestMissingObservedOutputLimitIsOmitted(t *testing.T) {
	raw, err := (&Client{}).buildBody(upstream.UpstreamRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if _, exists := body["max_tokens"]; exists {
		t.Fatalf("unobserved limit was invented: %s", raw)
	}
}

func TestObservedOutputBudgetPreservesExplicitLimit(t *testing.T) {
	c := &Client{account: &store.Account{WorkBuddyModelIDs: []string{`{"id":"deepseek-v4.1-flash","max_output_tokens":128000}`}}}
	// Both explicit zero and a small positive budget must reach the upstream.
	for _, want := range []int{0, 64} {
		limit := want
		raw, err := c.buildBody(upstream.UpstreamRequest{Model: "deepseek-v4.1-flash", MaxTokens: &limit})
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if body["max_tokens"] != float64(want) {
			t.Fatalf("explicit budget changed: %s", raw)
		}
	}
}

func TestDefaultOutputBudgetUsesSelectedAccountCatalog(t *testing.T) {
	for _, tt := range []struct {
		name, model string
		rows        []string
		want        int
	}{
		{"reasoning model", "deepseek-v4.1-flash", []string{`{"id":"deepseek-v4.1-flash","max_output_tokens":128000}`}, 128000},
		{"case insensitive", "DEEPSEEK-V4.1-FLASH", []string{`{"id":"Deepseek-V4.1-Flash","max_output_tokens":128000}`}, 128000},
		{"smaller model limit", "m", []string{`{"id":"m","max_output_tokens":4096}`}, 4096},
		{"different model", "other", []string{`{"id":"deepseek-v4.1-flash","max_output_tokens":128000}`}, 0},
		{"missing observation", "m", nil, 0},
		{"missing output limit", "m", []string{`{"id":"m","max_input_tokens":1000000}`}, 0},
		{"invalid output limit", "m", []string{`{"id":"m","max_output_tokens":-1}`, `{"id":"m","max_output_tokens":"unknown"}`, "m"}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := NewFromAccount(&store.Account{WorkBuddyModelIDs: tt.rows}, nil)
			defer c.Close()
			raw, err := c.buildBody(upstream.UpstreamRequest{Model: tt.model})
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			if tt.want == 0 {
				if _, exists := body["max_tokens"]; exists {
					t.Fatalf("unobserved limit was invented: %s", raw)
				}
				return
			}
			if body["max_tokens"] != float64(tt.want) {
				t.Fatalf("default budget = %v, want %d", body["max_tokens"], tt.want)
			}
		})
	}
}
