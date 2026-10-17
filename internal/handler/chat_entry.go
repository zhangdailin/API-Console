package handler

import (
	"fmt"
	"net/http"
	"orchids-api/internal/chatwire"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
)

// HandleChatRequest enters the same auth, selection, accounting and streaming
// pipeline as HTTP chat. raw retains the serialized size bound and fingerprint.
func (h *Handler) HandleChatRequest(w http.ResponseWriter, r *http.Request, chat chatwire.Request, raw []byte) {
	if len(raw) > maxRequestBytes {
		apperrors.New("invalid_request_error", "Request body too large", http.StatusRequestEntityTooLarge).WriteResponse(w)
		return
	}
	req, err := requestFromChat(chat)
	if err != nil {
		apperrors.New("invalid_request_error", "Invalid request body", http.StatusBadRequest).WriteResponse(w)
		return
	}
	h.handleMessages(w, r, &req, raw)
}

func requestFromChat(c chatwire.Request) (ClaudeRequest, error) {
	r := ClaudeRequest{Model: c.Model, Stream: c.Stream, MaxTokens: c.MaxTokens, MaxCompletionTokens: c.MaxCompletionTokens,
		Temperature: c.Temperature, TopP: c.TopP, Stop: c.Stop, ToolChoice: c.ToolChoice, ParallelToolCalls: c.ParallelToolCalls,
		ConversationID: c.ConversationID, Metadata: c.Metadata, ClientMetadata: c.ClientMetadata, OutputConfig: c.OutputConfig,
		ResponseFormat: c.ResponseFormat, ResponseText: c.ResponseText, Include: c.Include, PromptCacheKey: c.PromptCacheKey,
		ResponsesTools: c.ResponsesTools, MCPServers: c.MCPServers}
	if c.ReasoningEffort != nil {
		r.ReasoningEffort = *c.ReasoningEffort
	}
	for _, msg := range c.Messages {
		calls := make([]prompt.ChatToolCall, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			name, nameOK := call.Function["name"].(string)
			args, argsOK := call.Function["arguments"].(string)
			if (!nameOK && call.Function["name"] != nil) || (!argsOK && call.Function["arguments"] != nil) {
				return r, fmt.Errorf("tool name and arguments must be strings")
			}
			calls = append(calls, prompt.ChatToolCall{ID: call.ID, Type: call.Type, Function: prompt.ChatToolFunction{Name: name, Arguments: args}})
		}
		m, err := prompt.FromChat(msg.Role, msg.Content, msg.ReasoningContent, calls, msg.ToolCallID)
		if err != nil {
			return r, err
		}
		r.Messages = append(r.Messages, m)
	}
	for _, t := range c.Tools {
		tool := map[string]any{"type": t.Type}
		if len(t.Function) > 0 {
			tool["function"] = t.Function
		}
		r.Tools = append(r.Tools, tool)
	}
	return r, nil
}
