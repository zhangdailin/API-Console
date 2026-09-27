package upstream

import "orchids-api/internal/prompt"

// UpstreamRequest is the shared request representation for upstream providers.
type UpstreamRequest struct {
	Prompt            string
	Model             string
	Messages          []prompt.Message
	System            []prompt.SystemItem
	Tools             []interface{}
	ToolChoice        interface{}
	ParallelToolCalls *bool
	NoTools           bool
	Attempt           int
	// Optional generation controls retain explicit zero values; nil means omitted.
	MaxTokens   *int
	Temperature *float64
	TopP        *float64
	Stop        []string
	// ReasoningEffort is the OpenAI-style effort hint the client asked for
	// (reasoning_effort, or the Anthropic thinking/output_config dialect mapped
	// onto the same coarse levels). Providers whose wire contract exposes an
	// effort or thinking switch forward it; the value stays empty when the
	// client did not state one.
	ReasoningEffort string
	// RequestID identifies one downstream request across provider retries and
	// account switches. Providers that expose an upstream request-correlation
	// header can reuse it instead of making every retry look like a new turn.
	RequestID string
	// ConversationID is the explicit client conversation identifier. It stays
	// empty when the client did not provide one; providers must not substitute a
	// synthetic routing/session key for it.
	ConversationID string
	// TraceID preserves a client-provided trace independently from RequestID.
	// WorkBuddy uses RequestID to aggregate one turn and TraceID for diagnostics.
	TraceID       string
	ChatSessionID string
}

// SSEMessage is the shared streaming event representation for upstream providers.
type SSEMessage struct {
	Type  string                 `json:"type"`
	Event map[string]interface{} `json:"event,omitempty"`
}
