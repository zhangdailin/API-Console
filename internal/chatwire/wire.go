// Package chatwire holds the OpenAI Chat Completions wire types this gateway
// exchanges with its clients and between its own layers.
//
// The bridge accepts a Responses request, lowers it into this shape and posts it
// to the shared chat pipeline, so the type cannot belong to any one provider
// package. It is protocol shape only: no provider, transport or storage
// knowledge belongs here.
package chatwire

import (
	"encoding/json"
	"strings"
	"time"
)

// Request is the OpenAI Chat Completions request body.
type Request struct {
	Model               string           `json:"model"`
	Messages            []Message        `json:"messages"`
	Stream              bool             `json:"stream"`
	StreamProvided      bool             `json:"-"`
	Thinking            *string          `json:"thinking,omitempty"`
	ReasoningEffort     *string          `json:"reasoning_effort,omitempty"`
	ReasoningSummary    *string          `json:"reasoning_summary,omitempty"`
	Temperature         *float64         `json:"temperature,omitempty"`
	TopP                *float64         `json:"top_p,omitempty"`
	MaxTokens           *int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int             `json:"max_completion_tokens,omitempty"`
	ResponseFormat      map[string]any   `json:"response_format,omitempty"`
	SafetyIdentifier    string           `json:"safety_identifier,omitempty"`
	ResponseText        map[string]any   `json:"text,omitempty"`
	ResponsesTools      []map[string]any `json:"x_responses_tools,omitempty"`
	ResponsesInput      []any            `json:"x_responses_input,omitempty"`
	Include             []string         `json:"include,omitempty"`
	Tools               []ToolDef        `json:"tools,omitempty"`
	ToolChoice          any              `json:"tool_choice,omitempty"`
	// WebSearchOptions is the OpenAI-style switch for the hosted search tool.
	// It is lowered to a native web_search tool on the Responses planes.
	WebSearchOptions  map[string]any   `json:"web_search_options,omitempty"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls,omitempty"`
	Stop              []string         `json:"stop,omitempty"`
	PromptCacheKey    string           `json:"prompt_cache_key,omitempty"`
	MCPServers        []map[string]any `json:"mcp_servers,omitempty"`
	Metadata          map[string]any   `json:"metadata,omitempty"`
	ServiceTier       string           `json:"service_tier,omitempty"`
	OutputConfig      map[string]any   `json:"output_config,omitempty"`
	ThinkingConfig    map[string]any   `json:"thinking_config,omitempty"`
	ReasoningReplay   bool             `json:"-"`
	// StartedAt is stamped by the entrance that decoded the request, so every
	// latency measurement downstream measures the same origin.
	StartedAt time.Time `json:"-"`
	// SourceOperation names the endpoint the request arrived on ("chat",
	// "messages", "responses"). Several planes behave differently per entrance,
	// and it is carried on the request rather than derived again later.
	SourceOperation string `json:"-"`
}

type Message struct {
	Role                      string     `json:"role"`
	Content                   any        `json:"content"`
	ToolCalls                 []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID                string     `json:"tool_call_id,omitempty"`
	Name                      string     `json:"name,omitempty"`
	ReasoningContent          string     `json:"reasoning_content,omitempty"`
	ReasoningEncryptedContent string     `json:"reasoning_encrypted_content,omitempty"`
}

type ToolDef struct {
	Type     string         `json:"type"`
	Function map[string]any `json:"function,omitempty"`
	// Raw keeps every field of the declaration. Hosted tool types (web_search,
	// x_search, …) carry their own parameters outside `function`, and dropping
	// them would silently disable the feature the caller asked for.
	Raw map[string]any `json:"-"`
}

// UnmarshalJSON keeps the whole declaration so native (non-function) tools can
// be forwarded with their own fields intact.
func (t *ToolDef) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	t.Raw = raw
	t.Type = strings.TrimSpace(ParseLooseStringAny(raw["type"]))
	if fn, ok := raw["function"].(map[string]any); ok {
		t.Function = fn
	}
	return nil
}

type ToolCall struct {
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function map[string]any `json:"function,omitempty"`
}

// RateLimitInfo is the upstream quota window a response header carried.
type RateLimitInfo struct {
	Limit        int64
	HasLimit     bool
	Remaining    int64
	HasRemaining bool
	ResetAt      time.Time
	Unit         string
}
