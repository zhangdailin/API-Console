package grok

import (
	"bytes"
	"io"
	"net/http"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/util"
	"strings"

	"encoding/json"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

type anthropicMessagesRequest struct {
	Model         string                   `json:"model"`
	MaxTokens     int                      `json:"max_tokens"`
	Messages      []anthropicMessage       `json:"messages"`
	System        interface{}              `json:"system,omitempty"`
	Tools         []anthropicTool          `json:"tools,omitempty"`
	ToolChoice    interface{}              `json:"tool_choice,omitempty"`
	Stream        bool                     `json:"stream,omitempty"`
	Temperature   *float64                 `json:"temperature,omitempty"`
	TopP          *float64                 `json:"top_p,omitempty"`
	Metadata      map[string]interface{}   `json:"metadata,omitempty"`
	Thinking      map[string]interface{}   `json:"thinking,omitempty"`
	StopSequences []string                 `json:"stop_sequences,omitempty"`
	OutputConfig  map[string]interface{}   `json:"output_config,omitempty"`
	MCPServers    []map[string]interface{} `json:"mcp_servers,omitempty"`
}

type anthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type anthropicTool struct {
	Type            string      `json:"type,omitempty"`
	Name            string      `json:"name"`
	Description     string      `json:"description,omitempty"`
	InputSchema     interface{} `json:"input_schema"`
	Strict          *bool       `json:"strict,omitempty"`
	AllowedDomains  []string    `json:"allowed_domains,omitempty"`
	BlockedDomains  []string    `json:"blocked_domains,omitempty"`
	ExcludedDomains []string    `json:"excluded_domains,omitempty"`
}

// HandleMessages exposes an Anthropic Messages compatibility surface for the
// Grok channel while preserving the existing Chat routing and account logic.
func (h *Handler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req anthropicMessagesRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Model) == "" || len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "model and messages are required")
		return
	}
	req.Model = normalizeModelID(req.Model)
	if !middleware.APIKeyAllowsModel(r.Context(), req.Model) {
		writeAnthropicError(w, http.StatusForbidden, "API key is not allowed to use model "+req.Model)
		return
	}
	if req.MaxTokens <= 0 {
		writeAnthropicError(w, http.StatusBadRequest, "max_tokens must be greater than zero")
		return
	}
	spec, ok := h.resolveConversationModel(r.Context(), req.Model)
	if !ok {
		writeAnthropicModelNotFound(w, req.Model)
		return
	}
	if err := h.ensureResolvedModelCapability(r.Context(), spec.ID, spec, store.CapabilityMessages); err != nil {
		if strings.EqualFold(strings.TrimSpace(err.Error()), "model not found") {
			writeAnthropicModelNotFound(w, req.Model)
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, modelValidationMessage(req.Model, err))
		return
	}
	chat, err := anthropicRequestToChat(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := json.Marshal(chat)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "failed to encode request")
		return
	}
	subReq := r.Clone(chatwire.WithSourceOperation(r.Context(), "messages"))
	subReq.Method = http.MethodPost
	subReq.URL.Path = "/grok/v1/chat/completions"
	subReq.Header = r.Header.Clone()
	subReq.Header.Set("Content-Type", "application/json")
	subReq.Body = io.NopCloser(bytes.NewReader(body))
	subReq.ContentLength = int64(len(body))

	if req.Stream {
		prompt := estimatePromptUsageFromRequest(&chat)
		inputTokens := prompt.promptTextTokens + prompt.promptAudioTokens + prompt.promptImageTokens
		h.serveAnthropicMessageStream(w, subReq, req.Model, inputTokens)
		return
	}
	rec := httpserver.NewCaptureResponseWriter()
	h.HandleChatCompletions(rec, subReq)
	if rec.Code < 200 || rec.Code >= 300 {
		writeAnthropicUpstreamError(w, rec.Code, rec.Body.String())
		return
	}
	var chatResponse map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &chatResponse); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "invalid chat response")
		return
	}
	util.WriteJSON(w, anthropicResponseFromChat(req.Model, chatResponse))
}
