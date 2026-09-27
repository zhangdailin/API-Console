package qoder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// The chat request is a bespoke shape, not an OpenAI or Anthropic body. The
// fields below are the ones the gateway reads; `parameters` and the top-level
// `system` field are included because the CLI sends them and the gateway has
// been observed to fall back to them when the corresponding message is absent.

const (
	inferPath  = "/algo/api/v2/service/pro/sse/agent_chat_generation"
	inferQuery = "?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

	// Match the reference bridge's IDE request surface. This header and the
	// business.product field must agree for every signed chat call.
	sceneBusinessProduct = "ide"
	sceneBusinessType    = "agent"
	sceneName            = "assistant"

	chatTask    = "FREE_INPUT"
	sourceValue = 1
	taskID      = "common"
	agentID     = "agent_common"
	// sessionType matches the reference gateway's chat request. The upstream's
	// queue selection rules are not public; this value alone does not establish
	// why a 10605 refusal reports queueType "p3".
	sessionType = "qoder"

	// defaultAliyunUserType is the account class sent when the account's own
	// class is unknown. The reference gateway always sends one; an empty field
	// is not something the upstream observes from the IDE/CLI it emulates.
	defaultAliyunUserType = "personal_standard"
)

// aliyunUserTypeOr returns the reported account class, or the reference
// gateway's default if the class is unknown. Whether this value affects queue
// admission is not established by the 10605 response alone.
func aliyunUserTypeOr(aliyunUserType string) string {
	if trimmed := strings.TrimSpace(aliyunUserType); trimmed != "" {
		return trimmed
	}
	return defaultAliyunUserType
}

// chatURL renders the chat endpoint.
func chatURL(base string) string {
	return strings.TrimRight(base, "/") + inferPath + inferQuery
}

// chatBody is the request payload. Field order matters only for readability
// here: the body is encoded and the signature covers the encoded bytes, not the
// JSON, so member order is irrelevant to the signature.
type chatBody struct {
	Business          businessInfo           `json:"business"`
	RequestID         string                 `json:"request_id"`
	RequestSetID      string                 `json:"request_set_id"`
	ChatRecordID      string                 `json:"chat_record_id"`
	SessionID         string                 `json:"session_id"`
	Stream            bool                   `json:"stream"`
	ChatTask          string                 `json:"chat_task"`
	ChatContext       map[string]interface{} `json:"chat_context"`
	ImageURLs         interface{}            `json:"image_urls"`
	CodeLanguage      string                 `json:"code_language"`
	ChatPrompt        string                 `json:"chat_prompt"`
	IsReply           bool                   `json:"is_reply"`
	IsRetry           bool                   `json:"is_retry"`
	Source            int                    `json:"source"`
	Version           string                 `json:"version"`
	AgentID           string                 `json:"agent_id"`
	TaskID            string                 `json:"task_id"`
	SessionType       string                 `json:"session_type"`
	AliyunUser        string                 `json:"aliyun_user_type"`
	ModelConfig       modelConfigWire        `json:"model_config"`
	CustomModel       interface{}            `json:"custom_model"`
	System            string                 `json:"system"`
	Messages          []chatMessage          `json:"messages"`
	Tools             []interface{}          `json:"tools"`
	ToolChoice        interface{}            `json:"tool_choice,omitempty"`
	Parameters        map[string]interface{} `json:"parameters"`
	ParallelToolCalls *bool                  `json:"parallel_tool_calls,omitempty"`
}

// modelConfigWire matches the reference bridge's template keys. Its selected
// key, display name, capabilities and context bound come from the account's
// observed catalog rather than the reference template's hard-coded "auto".
type modelConfigWire struct {
	Key            string `json:"key"`
	DisplayName    string `json:"display_name"`
	Model          string `json:"model"`
	Format         string `json:"format"`
	IsVL           bool   `json:"is_vl"`
	IsReasoning    bool   `json:"is_reasoning"`
	APIKey         string `json:"api_key"`
	URL            string `json:"url"`
	Source         string `json:"source"`
	MaxInputTokens int    `json:"max_input_tokens"`
}

func wireModelConfig(model modelEntry, explicitReasoning bool) modelConfigWire {
	format := model.Format
	if format == "" {
		format = "openai"
	}
	source := model.Source
	if source == "" {
		source = "system"
	}
	return modelConfigWire{
		Key:            model.Key,
		DisplayName:    firstNonEmpty(model.DisplayName, model.Name, model.Key),
		Format:         format,
		Source:         source,
		IsVL:           model.IsVL,
		IsReasoning:    explicitReasoning,
		MaxInputTokens: model.MaxInputTokens,
	}
}

// businessInfo is the request's telemetry block.
type businessInfo struct {
	Product string `json:"product"`
	Version string `json:"version"`
	Type    string `json:"type"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	BeginAt int64  `json:"begin_at"`
	Stage   string `json:"stage"`
}

// chatMessage is one message in the upstream history.
type chatMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	Contents   []chatPart      `json:"contents,omitempty"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Reasoning  string          `json:"reasoning_content,omitempty"`
	Meta       json.RawMessage `json:"response_meta,omitempty"`
}

// chatPart is one content part of a multimodal user message.
type chatPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *chatImageURL   `json:"image_url,omitempty"`
	Source   json.RawMessage `json:"source,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

// chatToolCall is the OpenAI tool-call shape the gateway expects in history.
type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// buildChatBody renders the encoded request body.
func buildChatBody(req upstream.UpstreamRequest, model modelEntry, sessionID, requestID string) ([]byte, error) {
	return buildChatBodyVersion(req, model, sessionID, requestID, DefaultClientVersion)
}

func buildChatBodyVersion(req upstream.UpstreamRequest, model modelEntry, sessionID, requestID, clientVersion string) ([]byte, error) {
	return buildChatBodyScoped(req, model, sessionID, requestID, clientVersion, "")
}

// buildChatBodyScoped renders the encoded request body for one account class.
//
// aliyunUserType is the account's own class when it is known. The upstream
// sorts traffic by it, and an empty value is not a class it recognises, so a
// request that omits it is not queued with the account's real peers.
func buildChatBodyScoped(req upstream.UpstreamRequest, model modelEntry, sessionID, requestID, clientVersion, aliyunUserType string) ([]byte, error) {
	return buildChatBodyProfile(req, model, sessionID, requestID, clientVersion, aliyunUserType, sceneBusinessProduct)
}

func buildChatBodyProfile(req upstream.UpstreamRequest, model modelEntry, sessionID, requestID, clientVersion, aliyunUserType, businessProduct string) ([]byte, error) {
	messages, systemText, err := buildMessages(req)
	if err != nil {
		return nil, err
	}

	// Preserve explicit client controls; use the reference output budget only
	// when the caller omitted it.
	parameters := map[string]interface{}{"max_tokens": 32768}
	if req.MaxTokens != nil {
		parameters["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		parameters["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		parameters["top_p"] = *req.TopP
	}
	if req.Stop != nil {
		parameters["stop"] = append([]string{}, req.Stop...)
	}
	// The catalog's default tier is distinct from its input budget and largest
	// offered tier. Never opt into the largest tier implicitly.
	contextLength := model.ContextWindowInfo().DefaultContextTokens
	if contextLength <= 0 {
		contextLength = model.MaxInputTokens
	}
	if contextLength > 0 {
		parameters["context_length"] = contextLength
	}
	// The reference gateway leaves reasoning off by default even for a catalog
	// row marked is_reasoning=true; it enables model_config.is_reasoning only
	// when the caller explicitly requests thinking. Keep client-specified effort
	// intact so this changes only requests that supplied no reasoning preference.
	effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if effort != "" && effort != "none" {
		parameters["enable_thinking"] = true
		parameters["reasoning_effort"] = effort
	} else if effort == "none" {
		parameters["enable_thinking"] = false
	}
	tools := normalizeToolDefinitions(req, model)
	toolChoice, parallelTools := normalizeToolControls(req, len(tools) > 0)

	body := chatBody{
		Business: businessInfo{
			Product: businessProduct,
			Version: "1.1.3",
			Type:    sceneBusinessType,
			ID:      requestID,
			Name:    businessName(req),
			BeginAt: time.Now().UnixMilli(),
			Stage:   "start",
		},
		RequestID:         requestID,
		RequestSetID:      requestID,
		ChatRecordID:      requestID,
		SessionID:         sessionID,
		Stream:            true,
		ChatTask:          chatTask,
		ChatContext:       referenceChatContext(req, model),
		ImageURLs:         nil,
		CodeLanguage:      "",
		ChatPrompt:        "",
		IsReply:           true,
		IsRetry:           req.Attempt > 1,
		Source:            sourceValue,
		Version:           "3",
		AgentID:           agentID,
		TaskID:            taskID,
		SessionType:       sessionType,
		AliyunUser:        aliyunUserTypeOr(aliyunUserType),
		ModelConfig:       wireModelConfig(model, effort != "" && effort != "none"),
		CustomModel:       nil,
		System:            systemText,
		Messages:          messages,
		Tools:             tools,
		ToolChoice:        toolChoice,
		Parameters:        parameters,
		ParallelToolCalls: parallelTools,
	}
	if body.Tools == nil {
		// The gateway rejects a null tools array on some plans; an empty array is
		// the shape the CLI sends when the request carries no tools.
		body.Tools = []interface{}{}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal qoder request: %w", err)
	}
	return EncodeBody(raw), nil
}

// referenceChatContext mirrors the reference template's lightweight context
// metadata without importing its long built-in system prompt. The actual
// history remains in messages; only the latest user text is echoed here.
func referenceChatContext(req upstream.UpstreamRequest, model modelEntry) map[string]interface{} {
	prompt := latestUserText(req)
	return map[string]interface{}{
		"chatPrompt": "",
		"extra": map[string]interface{}{
			"context": []interface{}{},
			"modelConfig": map[string]interface{}{
				"is_reasoning": model.IsReasoning,
				"key":          model.Key,
			},
			"originalContent": map[string]interface{}{"type": "text", "text": prompt},
		},
		"features":  []interface{}{},
		"imageUrls": nil,
		"text":      map[string]interface{}{"type": "text", "text": prompt},
	}
}

func refreshedReplayBody(encoded []byte, requestID string) ([]byte, error) {
	raw, err := decodeBody(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode qoder replay body: %w", err)
	}
	var body chatBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("decode qoder replay JSON: %w", err)
	}
	body.RequestID = requestID
	body.RequestSetID = requestID
	body.ChatRecordID = requestID
	body.Business.ID = requestID
	body.Business.BeginAt = time.Now().UnixMilli()
	body.IsRetry = true
	updated, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal qoder replay body: %w", err)
	}
	return EncodeBody(updated), nil
}

// businessName is the first characters of the latest user text. The upstream
// uses it as a session label, so it is bounded and never a full prompt.
func businessName(req upstream.UpstreamRequest) string {
	text := strings.TrimSpace(latestUserText(req))
	if text == "" {
		text = strings.TrimSpace(req.Prompt)
	}
	runes := []rune(text)
	if len(runes) > 10 {
		runes = runes[:10]
	}
	return string(runes)
}

func latestUserText(req upstream.UpstreamRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg := req.Messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			continue
		}
		if msg.Content.IsString() {
			if text := strings.TrimSpace(msg.Content.GetText()); text != "" {
				return text
			}
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				return block.Text
			}
		}
	}
	return ""
}

// buildMessages renders the history. System items become a leading system
// message and are also returned separately for the top-level `system` field,
// because the gateway has been observed to read either.
//
// Tool results must survive as `tool` messages: rewriting them into text breaks
// the assistant/tool pairing and the upstream then rejects the history as
// malformed.
func buildMessages(req upstream.UpstreamRequest) ([]chatMessage, string, error) {
	systemParts := make([]string, 0, len(req.System)+1)
	out := make([]chatMessage, 0, len(req.Messages)+1)

	for _, item := range req.System {
		if text := strings.TrimSpace(item.Text); text != "" {
			systemParts = append(systemParts, text)
		}
	}

	toolCallIDs := map[string]bool{}
	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role == "developer" {
			// `developer` is the renamed system role; folding it keeps the
			// message instead of dropping it.
			role = "system"
		}
		if role == "system" && msg.Content.IsString() {
			if text := strings.TrimSpace(msg.Content.GetText()); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}

		if msg.Content.IsString() {
			text := msg.Content.GetText()
			if strings.TrimSpace(text) == "" {
				continue
			}
			message := chatMessage{Role: normalRole(role), Content: text}
			if role == "assistant" {
				message.Reasoning = strings.TrimSpace(msg.ReasoningContent)
			}
			out = append(out, message)
			continue
		}

		switch role {
		case "assistant":
			if message, ok := convertAssistantMessage(msg, toolCallIDs); ok {
				out = append(out, message)
			}
		default:
			out = append(out, convertBlockMessage(role, msg, toolCallIDs)...)
		}
	}

	systemText := strings.Join(systemParts, "\n")
	if systemText != "" {
		out = append([]chatMessage{{Role: "system", Content: systemText}}, out...)
	}
	if len(out) == 0 {
		text := strings.TrimSpace(req.Prompt)
		if text == "" {
			text = "Hello"
		}
		out = append(out, chatMessage{Role: "user", Content: text})
	}
	return out, systemText, nil
}

// normalRole folds the roles the gateway accepts. An unknown role becomes user
// rather than being forwarded, because the upstream rejects the whole request
// over one unrecognised role.
func normalRole(role string) string {
	switch role {
	case "assistant", "system", "tool":
		return role
	default:
		return "user"
	}
}

// convertAssistantMessage maps text, thinking and tool_use blocks onto one
// assistant message.
func convertAssistantMessage(msg prompt.Message, toolCallIDs map[string]bool) (chatMessage, bool) {
	message := chatMessage{Role: "assistant", Reasoning: strings.TrimSpace(msg.ReasoningContent)}
	texts := make([]string, 0, 2)
	for _, block := range msg.Content.GetBlocks() {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				texts = append(texts, block.Text)
			}
		case "thinking":
			if message.Reasoning == "" {
				message.Reasoning = strings.TrimSpace(block.Thinking)
			}
		case "tool_use":
			name := strings.TrimSpace(block.Name)
			if name == "" {
				continue
			}
			id := strings.TrimSpace(block.ID)
			if id == "" {
				id = NewToolCallID()
			}
			call := chatToolCall{ID: id, Type: "function"}
			call.Function.Name = name
			call.Function.Arguments = util.CompactToolInput(block.Input)
			message.ToolCalls = append(message.ToolCalls, call)
			toolCallIDs[id] = true
		}
	}
	message.Content = strings.Join(texts, "\n")
	if message.Content == "" && len(message.ToolCalls) == 0 && message.Reasoning == "" {
		return chatMessage{}, false
	}
	return message, true
}

// convertBlockMessage maps user/system blocks, splitting tool_result blocks into
// standalone `tool` messages so the assistant/tool pairing stays intact.
func convertBlockMessage(role string, msg prompt.Message, toolCallIDs map[string]bool) []chatMessage {
	blocks := msg.Content.GetBlocks()
	out := make([]chatMessage, 0, len(blocks))
	pendingParts := make([]chatPart, 0, len(blocks))
	hasImage := false

	flush := func() {
		if len(pendingParts) == 0 {
			return
		}
		message := chatMessage{Role: normalRole(role)}
		if !hasImage {
			texts := make([]string, 0, len(pendingParts))
			for _, part := range pendingParts {
				texts = append(texts, part.Text)
			}
			message.Content = strings.Join(texts, "\n")
		} else {
			// Preserve the original text/image interleaving. Transfer ownership
			// of this slice so the next segment cannot overwrite earlier parts.
			message.Contents = pendingParts
		}
		out = append(out, message)
		pendingParts = nil
		hasImage = false
	}

	for _, block := range blocks {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				pendingParts = append(pendingParts, chatPart{Type: "text", Text: block.Text})
			}
		case "image":
			if url := blockImageURL(block); url != "" {
				pendingParts = append(pendingParts, chatPart{Type: "image_url", ImageURL: &chatImageURL{URL: url}})
				hasImage = true
			}
		case "tool_result":
			flush()
			toolID := strings.TrimSpace(block.ToolUseID)
			if toolID == "" {
				continue
			}
			// A result whose call never appeared in this history would leave a
			// dangling tool message, which the upstream rejects. The pairing is
			// tracked so it can be reported instead of silently dropped.
			if !toolCallIDs[toolID] {
				continue
			}
			delete(toolCallIDs, toolID)
			out = append(out, chatMessage{
				Role:       "tool",
				ToolCallID: toolID,
				Content:    stringifyToolResult(block.Content),
			})
		}
	}
	flush()
	return out
}

// blockImageURL extracts the image reference from a content block. Both the
// Anthropic `source` shape and a plain URL are accepted; the service forwards
// the reference rather than downloading the image.
func blockImageURL(block prompt.ContentBlock) string {
	if block.Source != nil {
		if url := strings.TrimSpace(block.Source.URL); url != "" {
			return url
		}
		if data := strings.TrimSpace(block.Source.Data); data != "" {
			mediaType := strings.TrimSpace(block.Source.MediaType)
			if mediaType == "" {
				mediaType = "image/png"
			}
			return "data:" + mediaType + ";base64," + data
		}
	}
	return strings.TrimSpace(block.URL)
}

// stringifyToolResult flattens a tool result onto the string the gateway
// expects. A structured result is serialized rather than dropped: losing it
// would leave the model reasoning about a tool that returned nothing.
func stringifyToolResult(value interface{}) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []prompt.ContentBlock:
		parts := make([]string, 0, len(typed))
		for _, block := range typed {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(raw)
	}
}

// normalizeToolDefinitions accepts both OpenAI
// (`{"type":"function","function":{...}}`) and Anthropic
// (`{"name":...,"input_schema":...}`) declarations and renders the OpenAI shape.
func normalizeToolDefinitions(req upstream.UpstreamRequest, model modelEntry) []interface{} {
	if req.NoTools || len(req.Tools) == 0 {
		return nil
	}
	out := make([]interface{}, 0, len(req.Tools))
	for _, tool := range req.Tools {
		raw, err := json.Marshal(tool)
		if err != nil {
			continue
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			continue
		}
		if fn, ok := decoded["function"].(map[string]interface{}); ok {
			if strings.TrimSpace(util.StringValue(fn["name"])) == "" {
				continue
			}
			decoded["type"] = "function"
			out = append(out, decoded)
			continue
		}
		name := strings.TrimSpace(util.StringValue(decoded["name"]))
		if name == "" {
			continue
		}
		parameters := decoded["input_schema"]
		if parameters == nil {
			parameters = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": util.StringValue(decoded["description"]),
				"parameters":  parameters,
			},
		})
	}
	_ = model
	return out
}

// normalizeToolControls maps both OpenAI and Anthropic tool selection shapes to
// the OpenAI-compatible fields accepted by Qoder's chat endpoint. Tool controls
// belong at the top level of the request; placing tool_choice under parameters
// makes the gateway treat an otherwise valid tool request as ordinary chat.
func normalizeToolControls(req upstream.UpstreamRequest, toolsEnabled bool) (interface{}, *bool) {
	if !toolsEnabled || req.NoTools {
		return nil, nil
	}

	parallel := cloneBool(req.ParallelToolCalls)
	choice := req.ToolChoice
	if choice == nil {
		return "auto", parallel
	}

	switch typed := choice.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "auto", "required", "none":
			return strings.ToLower(strings.TrimSpace(typed)), parallel
		default:
			return "auto", parallel
		}
	case map[string]interface{}:
		kind := strings.ToLower(strings.TrimSpace(util.StringValue(typed["type"])))
		if disable, ok := typed["disable_parallel_tool_use"].(bool); ok && parallel == nil {
			value := !disable
			parallel = &value
		}
		switch kind {
		case "auto":
			return "auto", parallel
		case "any", "required":
			return "required", parallel
		case "none":
			return "none", parallel
		case "tool":
			name := strings.TrimSpace(util.StringValue(typed["name"]))
			if name == "" {
				return "auto", parallel
			}
			return map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": name},
			}, parallel
		case "function":
			return typed, parallel
		default:
			return "auto", parallel
		}
	default:
		return "auto", parallel
	}
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// applyAuthHeaders sets the signed header set on an inference request.
//
// The header count is conditional: the organization headers are omitted when
// the account has no organization, and the model headers when no model key
// applies. Presence is not cosmetic — the gateway rejects a request that
// carries an empty organization header.
func (c *Client) applyAuthHeaders(req *http.Request, creds Credentials, fields RuntimeFields, requestID, modelKey, modelSource, body, signedPath string) error {
	payloadBase64, err := buildCOSYPayload(requestID, fields.EncryptUserInfo, c.clientVersion)
	if err != nil {
		return err
	}
	unixSeconds := strconv.FormatInt(time.Now().Unix(), 10)
	signature := signRequest(payloadBase64, fields.Key, unixSeconds, body, signedPath)

	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", composeBearer(payloadBase64, signature))
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cosy-Business-Product", c.businessProduct())
	req.Header.Set("Cosy-Business-Type", sceneBusinessType)
	req.Header.Set("Cosy-ClientType", sceneClientID)
	req.Header.Set("Cosy-Data-Policy", dataPolicyHeader(c.dataPolicyAgreed()))
	req.Header.Set("Cosy-Date", unixSeconds)
	req.Header.Set("Cosy-Key", fields.Key)
	req.Header.Set("Cosy-MachineId", c.machineID)
	req.Header.Set("Cosy-MachineToken", c.machineTokenOr(c.machineID))
	req.Header.Set("Cosy-MachineType", c.machineTypeOr(sceneClientID))
	if orgID := strings.TrimSpace(creds.OrgID); orgID != "" {
		req.Header.Set("Cosy-Organization-Id", orgID)
	}
	if tags := filterTags(creds.OrgTags); len(tags) > 0 {
		req.Header.Set("Cosy-Organization-Tags", strings.Join(tags, ","))
	}
	req.Header.Set("Cosy-Scene", sceneName)
	req.Header.Set("Cosy-User", strings.TrimSpace(creds.UID))
	req.Header.Set("Cosy-Version", c.clientVersion)
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	req.Header.Set("Login-Version", "v2")
	if key := strings.TrimSpace(modelKey); key != "" {
		req.Header.Set("X-Model-Key", key)
		// The source header is gated on the key, not on its own value: the CLI
		// sends it even when the source itself is empty.
		req.Header.Set("X-Model-Source", strings.TrimSpace(modelSource))
	}
	return nil
}

func dataPolicyHeader(agreed bool) string {
	if agreed {
		return "agree"
	}
	return "disagree"
}

func filterTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if trimmed := strings.TrimSpace(tag); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// attemptChat performs one upstream attempt and consumes its stream.
func (c *Client) attemptChat(ctx context.Context, url string, body []byte, model modelEntry, requestID string, fields RuntimeFields, creds Credentials, toolsEnabled bool, emit func(upstream.SSEMessage)) (streamResult, error) {
	reqCtx, cancel := util.WithDefaultTimeout(ctx, c.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return streamResult{}, &attemptStreamError{err: fmt.Errorf("build qoder request: %w", err)}
	}
	if err := c.applyAuthHeaders(req, creds, fields, requestID, model.Key, model.Source, string(body), signPath(url)); err != nil {
		return streamResult{}, &attemptStreamError{err: err}
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.stream.Do(req)
	if err != nil {
		// Only a connection-level hiccup is worth another attempt; a bad URL or
		// an untrusted certificate would fail identically every time.
		return streamResult{}, &attemptStreamError{err: fmt.Errorf("send qoder request: %w", err), retryable: IsTransientTransport(err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return streamResult{}, classifyStatus(resp.StatusCode, resp.Header.Get("Retry-After"), raw)
	}

	result, err := consumeStreamWithTools(resp.Body, toolsEnabled, emit)
	if err != nil {
		var target *attemptStreamError
		if asAttemptError(err, &target) {
			return result, err
		}
		// A busy or unauthorized verdict that arrived as an in-stream frame is
		// still a retry decision.
		switch {
		case errors.Is(err, ErrBusy):
			return result, &attemptStreamError{err: err, retryable: true, busy: true, wait: 2 * time.Second}
		case errors.Is(err, errUpstreamUnauthorized):
			return result, &attemptStreamError{err: err, unauth: true}
		}
		return result, err
	}
	return result, nil
}

// classifyStatus turns an HTTP failure into a retry decision. The gateway
// reports its queue refusal as business code 10605 under a 401, so the code is
// read before the status: refreshing on a busy verdict would burn the account's
// token for nothing.
func classifyStatus(status int, retryAfter string, raw []byte) error {
	code := envelopeCode(raw)
	detail := extractBodyMessage(raw)
	if detail == "" {
		detail = truncate(strings.TrimSpace(string(raw)), 300)
	}
	wrapped := apiError(http.MethodPost, "chat", status, raw)

	if code == busyCode || sharedQueueRefusal(detail, string(raw)) {
		return &attemptStreamError{err: fmt.Errorf("%w: %v", ErrBusy, wrapped), busy: true, retryable: true, wait: busyWait(retryAfter, raw)}
	}
	// A 403 that names the pricing page is an entitlement refusal, not a
	// credential failure: retrying and refreshing both change nothing, and
	// classifying it as unauthorized would retire a valid account.
	if DetectNoEntitlement(detail, string(raw)) {
		return &attemptStreamError{err: entitlementError(string(raw))}
	}
	// Business verdicts take precedence over HTTP authentication statuses,
	// just as they do inside an SSE envelope.
	if isDuplicateRequest(detail, string(raw)) {
		return fmt.Errorf("qoder duplicate request")
	}
	if hasAgentLimitReset(detail, string(raw)) {
		return &agentLimitError{resetAt: agentLimitResetAt(detail, string(raw))}
	}
	if IsContentPolicy(string(raw)) {
		return &attemptStreamError{err: contentPolicyError(wrapped.Error())}
	}
	if IsClientFault(string(raw)) {
		return &attemptStreamError{err: fmt.Errorf("%w: %v", ErrClientFault, wrapped)}
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return &attemptStreamError{err: fmt.Errorf("%w: %v", errUpstreamUnauthorized, wrapped), unauth: true}
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return &attemptStreamError{err: wrapped, retryable: true, wait: retryAfterDelay(retryAfter)}
	}
	// A provider-side hiccup wearing any status is worth one bounded retry on
	// the account that already holds the request.
	if IsTransientUpstreamStatus(status, string(raw)) {
		return &attemptStreamError{err: transientError(wrapped.Error()), retryable: true, wait: retryAfterDelay(retryAfter)}
	}
	if status >= 500 {
		return &attemptStreamError{err: wrapped, retryable: true}
	}
	// The rest are request-side refusals: replaying them changes nothing and
	// only takes a healthy account out of rotation.
	_ = detail
	return &attemptStreamError{err: wrapped}
}

// busyWait reads the gateway's own backoff hint, capped so a hostile or buggy
// value cannot park a request indefinitely.
func busyWait(retryAfter string, raw []byte) time.Duration {
	if delay := retryAfterDelay(retryAfter); delay > 0 {
		return delay
	}
	if seconds := nestedInt64(raw, "retryAfterSeconds", 0); seconds > 0 {
		return capWait(time.Duration(seconds) * time.Second)
	}
	if millis := nestedInt64(raw, "retryAfterMs", 0); millis > 0 {
		return capWait(time.Duration(millis) * time.Millisecond)
	}
	if wait := nestedInt64(raw, "waitTime", 0); wait > 0 {
		// waitTime has historically been milliseconds, while the explicitly named
		// retryAfterSeconds is seconds.
		return capWait(time.Duration(wait) * time.Millisecond)
	}
	return 2 * time.Second
}

func nestedInt64(raw []byte, key string, depth int) int64 {
	if depth > 5 || len(bytes.TrimSpace(raw)) == 0 {
		return 0
	}
	var value interface{}
	if json.Unmarshal(bytes.TrimSpace(raw), &value) != nil {
		return 0
	}
	var walk func(interface{}, int) int64
	walk = func(node interface{}, level int) int64 {
		if level > 5 {
			return 0
		}
		switch typed := node.(type) {
		case map[string]interface{}:
			if rawValue, ok := typed[key]; ok {
				switch number := rawValue.(type) {
				case float64:
					return int64(number)
				case string:
					parsed, _ := strconv.ParseInt(strings.TrimSpace(number), 10, 64)
					return parsed
				}
			}
			for _, child := range typed {
				if found := walk(child, level+1); found > 0 {
					return found
				}
			}
		case []interface{}:
			for _, child := range typed {
				if found := walk(child, level+1); found > 0 {
					return found
				}
			}
		case string:
			var nested interface{}
			if json.Unmarshal([]byte(strings.TrimSpace(typed)), &nested) == nil {
				return walk(nested, level+1)
			}
		}
		return 0
	}
	return walk(value, depth)
}

func retryAfterDelay(value string) time.Duration {
	return retryAfterDelayAt(value, time.Now())
}

func retryAfterDelayAt(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds >= int64(30*time.Second/time.Second) {
			return 30 * time.Second
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return capWait(at.Sub(now))
	}
	return 0
}

func capWait(wait time.Duration) time.Duration {
	if wait > 30*time.Second {
		return 30 * time.Second
	}
	if wait < 0 {
		return 0
	}
	return wait
}
