package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

func TestEstimateInputTokenBreakdown_SplitsSystemContext(t *testing.T) {
	t.Parallel()

	prompt := "<env>\ndate: 2026-02-12\n</env>\n<rules>\n- concise\n</rules>\n<sys>\nproject context and constraints\n</sys>\n<user>\nhello\n</user>"
	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":       "Read",
				"parameters": map[string]any{"type": "object"},
			},
		},
	}

	bd := estimateInputTokenBreakdown(prompt, tools)
	testutil.False(t, bd.SystemContextTokens <= 0, "expected system_context tokens > 0")
	testutil.False(t, bd.BasePromptTokens <= 0, "expected base prompt tokens > 0")
	testutil.False(t, bd.ToolsTokens <= 0, "expected tools tokens > 0")
	testutil.Equal(t, bd.Total, bd.BasePromptTokens+bd.SystemContextTokens+bd.HistoryTokens+bd.ToolsTokens)
}

func TestEstimateInputTokenBreakdown_ProductionFallbackAlwaysPositive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		prompt string
		tools  []any
	}{
		{name: "empty user text uses request placeholder", prompt: "request"},
		{name: "image-only user text uses request placeholder", prompt: "request"},
		{name: "passthrough empty text", prompt: "workbuddy request"},
		{name: "only tools", prompt: "request", tools: []any{map[string]any{"name": "Read"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := estimateInputTokenBreakdown(tc.prompt, tc.tools)
			testutil.Falsef(t, got.Total < 1, "builtPrompt=%q tools=%v: total=%d, want positive", tc.prompt, tc.tools, got.Total)
		})
	}
}

func TestHandleCountTokens_ReturnsBreakdown(t *testing.T) {
	t.Parallel()

	h := NewWithLoadBalancer(&config.Config{
		DebugEnabled:   false,
		DebugLogSSE:    false,
		RequestTimeout: 30,
	}, nil)

	reqBody := map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "What is dependency injection?"}},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":       "Read",
					"parameters": map[string]any{"type": "object"},
				},
			},
		},
	}
	raw, _ := json.Marshal(reqBody)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/v1/messages/count_tokens", bytes.NewReader(raw))

	h.HandleCountTokens(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	var resp map[string]any
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode response: %v")
	v, ok := resp["input_tokens"].(float64)
	testutil.Falsef(t, !ok || v <= 0, "expected positive input_tokens, got %#v", resp["input_tokens"])
	_, ok = resp["prompt_profile"].(string)
	testutil.Falsef(t, !ok, "expected prompt_profile string, got %#v", resp["prompt_profile"])
	breakdown, ok := resp["breakdown"].(map[string]any)
	testutil.True(t, ok, "expected breakdown object, got %#v")
	required := []string{"base_prompt_tokens", "system_context_tokens", "history_tokens", "tools_tokens"}
	for _, key := range required {
		_, ok := breakdown[key].(float64)
		testutil.Falsef(t, !ok, "expected breakdown key %q as number, got %#v", key, breakdown[key])
	}
	toolsTokens, _ := breakdown["tools_tokens"].(float64)
	testutil.Falsef(t, toolsTokens <= 0, "expected positive tools_tokens, got %#v", breakdown["tools_tokens"])
}
