package handler

import "orchids-api/internal/prompt"

type ClaudeRequest struct {
	MaxTokens           *int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64         `json:"temperature,omitempty"`
	TopP                *float64         `json:"top_p,omitempty"`
	Stop                StopSequences    `json:"stop,omitempty"`
	StopSequences       []string         `json:"stop_sequences,omitempty"`
	Model               string           `json:"model"`
	Messages            []prompt.Message `json:"messages"`
	System              SystemItems      `json:"system"`
	Tools               []any            `json:"tools"`
	ToolChoice          any              `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool            `json:"parallel_tool_calls,omitempty"`
	Stream              bool             `json:"stream"`
	ConversationID      string           `json:"conversation_id"`
	ConversationIDAlt   string           `json:"conversationId"`
	Metadata            map[string]any   `json:"metadata"`
	ClientMetadata      map[string]any   `json:"client_metadata,omitempty"`
	// ReasoningEffort is the OpenAI-style effort hint. A catalog publishes models
	// as "<family>-<effort>", so a client that asks for the family name plus an
	// effort must have it resolved onto the catalog entry.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// OutputConfig and Thinking carry the Anthropic-side effort hints Claude
	// Code sends (output_config.effort, thinking.effort/budget_tokens). They
	// feed the same effort resolution as reasoning_effort.
	OutputConfig   map[string]any   `json:"output_config,omitempty"`
	Thinking       map[string]any   `json:"thinking,omitempty"`
	ResponseFormat map[string]any   `json:"response_format,omitempty"`
	ResponseText   map[string]any   `json:"text,omitempty"`
	Include        []string         `json:"include,omitempty"`
	PromptCacheKey string           `json:"prompt_cache_key,omitempty"`
	ResponsesTools []map[string]any `json:"x_responses_tools,omitempty"`
	MCPServers     []map[string]any `json:"mcp_servers,omitempty"`
}

type openAINonStreamToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAINonStreamMessage struct {
	Role             string                    `json:"role"`
	Content          any                       `json:"content"`
	ReasoningContent string                    `json:"reasoning_content,omitempty"`
	ToolCalls        []openAINonStreamToolCall `json:"tool_calls,omitempty"`
}

type openAINonStreamChoice struct {
	Index        int                    `json:"index"`
	Message      openAINonStreamMessage `json:"message"`
	FinishReason *string                `json:"finish_reason"`
}

type openAINonStreamResponse struct {
	ID      string                  `json:"id"`
	Object  string                  `json:"object"`
	Created int64                   `json:"created"`
	Model   string                  `json:"model"`
	Choices []openAINonStreamChoice `json:"choices"`
	Usage   openAINonStreamUsage    `json:"usage"`
}
