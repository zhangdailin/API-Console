package qoder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"orchids-api/internal/httpclient"
	"strconv"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/debug"
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

	// sceneBusinessProduct, sceneName and sessionType are the QoderWork client
	// identity taken from a packet capture of the international client. They
	// replaced the IDE emulation this channel was built around (product "ide",
	// scene "assistant", session_type "qoder"). business.version and the
	// Cosy-Version header both carry the configured client version, which is
	// what the capture shows for those two fields.
	sceneBusinessProduct = "qoder_work"
	sceneBusinessType    = "agent"
	sceneName            = "qwork"

	// machineSceneType is Cosy-MachineType. The capture reports client type 6
	// together with machine type 5 in one request, so this cannot share the
	// sceneClientID constant the way it used to.
	machineSceneType = "5"
	// machineOS is Cosy-MachineOS, which the QoderWork client always sends.
	machineOS = "x86_64_win32"
	// clientUserAgent matches the QoderWork client, which is a Node program
	// rather than the Go default HTTP client this channel used to advertise.
	clientUserAgent = "node"
	// subTask is business.sub_task. The capture reports the interactive agent
	// session as "ws_builtin_general"; the internal reflection agent uses a
	// different value that does not describe a proxied user chat.
	subTask = "ws_builtin_general"

	chatTask    = "FREE_INPUT"
	sourceValue = 1
	taskID      = "common"
	agentID     = "agent_common"
	// sessionType is the QoderWork session type from the capture.
	sessionType = "qoder_work"

	// defaultAliyunUserType is the account class used where a class is required
	// but the account reported none. The chat body no longer falls back to it:
	// the capture shows the QoderWork client sending aliyun_user_type empty.
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
func chatURL(base string) string { return strings.TrimRight(base, "/") + inferPath + inferQuery }

// chatBody is the request payload. Field order matters only for readability
// here: the body is encoded and the signature covers the encoded bytes, not the
// JSON, so member order is irrelevant to the signature.
//
// image_urls, code_language, chat_prompt and custom_model are deliberately
// absent. This channel used to send all four as empty values; a capture of the
// QoderWork client shows it sends none of them.
type chatBody struct {
	Business          businessInfo    `json:"business"`
	RequestID         string          `json:"request_id"`
	RequestSetID      string          `json:"request_set_id"`
	ChatRecordID      string          `json:"chat_record_id"`
	SessionID         string          `json:"session_id"`
	Stream            bool            `json:"stream"`
	ChatTask          string          `json:"chat_task"`
	ChatContext       interface{}     `json:"chat_context"`
	IsReply           bool            `json:"is_reply"`
	IsRetry           bool            `json:"is_retry"`
	Source            int             `json:"source"`
	Version           string          `json:"version"`
	AgentID           string          `json:"agent_id"`
	TaskID            string          `json:"task_id"`
	SessionType       string          `json:"session_type"`
	AliyunUser        string          `json:"aliyun_user_type"`
	ModelConfig       modelConfigWire `json:"model_config"`
	System            string          `json:"system"`
	Messages          []chatMessage   `json:"messages"`
	Tools             []interface{}   `json:"tools"`
	ToolChoice        interface{}     `json:"tool_choice,omitempty"`
	Parameters        interface{}     `json:"parameters"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
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

func wireModelConfig(model modelEntry) modelConfigWire {
	format := model.Format
	format = util.FirstNonEmptyUntrimmed(format, "openai")
	source := model.Source
	source = util.FirstNonEmptyUntrimmed(source, "system")
	return modelConfigWire{
		Key:            model.Key,
		DisplayName:    util.FirstNonEmpty(model.DisplayName, model.Name, model.Key),
		Format:         format,
		Source:         source,
		IsVL:           model.IsVL,
		IsReasoning:    model.IsReasoning,
		MaxInputTokens: model.MaxInputTokens,
	}
}

// businessInfo is the request's telemetry block. The capture carries a
// sub_task the earlier shape omitted, and business.id is the task set id rather
// than the request id.
type businessInfo struct {
	Product string `json:"product"`
	Version string `json:"version"`
	Type    string `json:"type"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	BeginAt int64  `json:"begin_at"`
	Stage   string `json:"stage"`
	SubTask string `json:"sub_task"`
}

// chatMessage is one message in the upstream history.
type chatMessage struct {
	Role          string          `json:"role"`
	Content       string          `json:"content,omitempty"`
	Contents      []chatPart      `json:"contents,omitempty"`
	ToolCalls     []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID    string          `json:"tool_call_id,omitempty"`
	Name          string          `json:"name,omitempty"`
	Reasoning     string          `json:"reasoning_content,omitempty"`
	Meta          json.RawMessage `json:"response_meta,omitempty"`
	ReasoningItem json.RawMessage `json:"reasoning_item,omitempty"`
}

// chatPart is one content part of a multimodal user message.
type chatPart struct {
	CacheControl *prompt.CacheControl `json:"cache_control,omitempty"`
	Type         string               `json:"type"`
	Text         string               `json:"text,omitempty"`
	ImageURL     *chatImageURL        `json:"image_url,omitempty"`
	Source       json.RawMessage      `json:"source,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

// chatToolCall is the OpenAI tool-call shape the gateway expects in history.
type chatToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatParameters struct {
	ResponseFormat  map[string]interface{} `json:"response_format,omitempty"`
	MaxTokens       int                    `json:"max_tokens"`
	Temperature     *float64               `json:"temperature,omitempty"`
	TopP            *float64               `json:"top_p,omitempty"`
	Stop            *[]string              `json:"stop,omitempty"`
	ContextLength   int                    `json:"context_length,omitempty"`
	EnableThinking  *bool                  `json:"enable_thinking,omitempty"`
	ReasoningEffort string                 `json:"reasoning_effort,omitempty"`
}

func buildChatBodyProfile(req upstream.UpstreamRequest, model modelEntry, sessionID, requestID, requestSetID, clientVersion, aliyunUserType, businessProduct string) ([]byte, error) {
	if err := req.ValidateProtocolControls("qoder"); err != nil {
		return nil, err
	}
	messages, systemText, err := buildMessages(req)
	if err != nil {
		return nil, err
	}

	// Preserve explicit client controls; use the reference output budget only
	// when the caller omitted it.
	parameters := chatParameters{MaxTokens: 32000, Temperature: req.Temperature, TopP: req.TopP}
	parameters.ResponseFormat = req.ChatResponseFormat()
	if req.MaxTokens != nil {
		parameters.MaxTokens = *req.MaxTokens
	}
	if req.Stop != nil {
		stops := append([]string{}, req.Stop...)
		parameters.Stop = &stops
	}
	// The catalog's default tier is distinct from its input budget and largest
	// offered tier. Never opt into the largest tier implicitly.
	contextLength := model.ContextWindowInfo().DefaultContextTokens
	if contextLength <= 0 {
		contextLength = model.MaxInputTokens
	}
	if contextLength > 0 {
		parameters.ContextLength = contextLength
	}
	// The reference gateway leaves reasoning off by default even for a catalog
	// row marked is_reasoning=true; it enables model_config.is_reasoning only
	// when the caller explicitly requests thinking. Keep client-specified effort
	// intact so this changes only requests that supplied no reasoning preference.
	effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if effort != "" && effort != "none" {
		thinking := true
		parameters.EnableThinking = &thinking
		parameters.ReasoningEffort = effort
	} else if effort == "none" {
		thinking := false
		parameters.EnableThinking = &thinking
	}
	tools := normalizeToolDefinitions(req, model)
	toolChoice, parallelTools := normalizeToolControls(req, len(tools) > 0)

	body := chatBody{
		Business: businessInfo{
			Product: businessProduct,
			Version: clientVersion,
			Type:    sceneBusinessType,
			ID:      requestSetID,
			Name:    businessName(req),
			BeginAt: time.Now().UnixMilli(),
			Stage:   "start",
			SubTask: subTask,
		},
		RequestID:         requestID,
		RequestSetID:      requestSetID,
		ChatRecordID:      requestID,
		SessionID:         sessionID,
		Stream:            true,
		ChatTask:          chatTask,
		ChatContext:       referenceChatContext(req, model),
		IsReply:           true,
		IsRetry:           false, // Official queued attempts keep this false; Attempt is local bookkeeping.
		Source:            sourceValue,
		Version:           "3",
		AgentID:           agentID,
		TaskID:            taskID,
		SessionType:       sessionType,
		AliyunUser:        strings.TrimSpace(aliyunUserType),
		ModelConfig:       wireModelConfig(model),
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

	encoded, err := marshalEncodedBody(body)
	if err != nil {
		return nil, fmt.Errorf("marshal qoder request: %w", err)
	}
	return encoded, nil
}

// referenceChatContext mirrors the capture's lightweight context metadata
// without importing its long built-in system prompt. The actual history remains
// in messages.
//
// text and extra.originalContent both carry the same plain string in the
// capture. This channel used to send them as {"type":"text","text":...}
// objects, a shape the QoderWork client never produces.
type referenceContext struct {
	ChatPrompt string `json:"chatPrompt"`
	Extra      struct {
		Context     []interface{} `json:"context"`
		ModelConfig struct {
			IsReasoning bool   `json:"is_reasoning"`
			Key         string `json:"key"`
		} `json:"modelConfig"`
		OriginalContent string `json:"originalContent"`
	} `json:"extra"`
	Features  []interface{} `json:"features"`
	ImageURLs interface{}   `json:"imageUrls"`
	Text      string        `json:"text"`
}

func referenceChatContext(req upstream.UpstreamRequest, model modelEntry) referenceContext {
	prompt := latestUserText(req)
	ctx := referenceContext{Features: []interface{}{}, Text: prompt}
	ctx.Extra.Context = []interface{}{}
	ctx.Extra.ModelConfig.IsReasoning, ctx.Extra.ModelConfig.Key = model.IsReasoning, model.Key
	ctx.Extra.OriginalContent = prompt
	return ctx
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
	body.ChatRecordID = requestID
	// request_set_id and business.id identify the task, not the attempt: the
	// capture keeps them constant while request_id changes between the requests
	// of one task. A replay is the same task, so they stay put.
	body.Business.BeginAt = time.Now().UnixMilli()
	body.IsRetry = false
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
	return strings.TrimSpace(req.Prompt)
}

// Scope explicit conversation identities to the account without retaining raw IDs.
func conversationSessionID(uid, conversation string) string {
	sum := sha256.Sum256([]byte("qoder-session\x00" + uid + "\x00" + conversation))
	sum[6] = (sum[6] & 0x0f) | 0x80
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
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
				if role == "assistant" && (strings.TrimSpace(msg.ReasoningContent) != "" || len(msg.ReasoningItem) > 0) {
					out = append(out, chatMessage{Role: role, Reasoning: msg.ReasoningContent, ReasoningItem: msg.ReasoningItem})
				}
				continue
			}
			message := chatMessage{Role: normalRole(role), Content: text}
			if role == "assistant" {
				message.Reasoning = msg.ReasoningContent
				message.ReasoningItem = msg.ReasoningItem
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
	message := chatMessage{Role: "assistant", Reasoning: msg.ReasoningContent, ReasoningItem: msg.ReasoningItem}
	texts := make([]string, 0, 2)
	for _, block := range msg.Content.GetBlocks() {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				texts = append(texts, block.Text)
			}
		case "thinking":
			if message.Reasoning == "" {
				message.Reasoning = block.Thinking
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
			call := chatToolCall{ID: id, Type: "function", Index: block.ToolIndex}
			call.Function.Name = name
			call.Function.Arguments = util.CompactToolInput(block.Input)
			message.ToolCalls = append(message.ToolCalls, call)
			toolCallIDs[id] = true
		}
	}
	message.Content = strings.Join(texts, "\n")
	if message.Content == "" && len(message.ToolCalls) == 0 && message.Reasoning == "" && len(message.ReasoningItem) == 0 {
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
	preserveParts := false

	flush := func() {
		if len(pendingParts) == 0 {
			return
		}
		message := chatMessage{Role: normalRole(role)}
		if !preserveParts {
			texts := make([]string, 0, len(pendingParts))
			for _, part := range pendingParts {
				texts = append(texts, part.Text)
			}
			message.Content = strings.Join(texts, "\n")
		} else {
			// Preserve text/image interleaving and per-part cache hints. Transfer ownership
			// of this slice so the next segment cannot overwrite earlier parts.
			message.Contents = pendingParts
		}
		out = append(out, message)
		pendingParts = nil
		preserveParts = false
	}

	for _, block := range blocks {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				pendingParts = append(pendingParts, chatPart{Type: "text", Text: block.Text, CacheControl: block.CacheControl})
				if block.CacheControl != nil {
					preserveParts = true
				}
			}
		case "image":
			if url := blockImageURL(block); url != "" {
				pendingParts = append(pendingParts, chatPart{Type: "image_url", ImageURL: &chatImageURL{URL: url}, CacheControl: block.CacheControl})
				preserveParts = true
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
				Content:    util.StringifyToolResult(block.Content),
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

// normalizeToolDefinitions renders the OpenAI function envelope for the
// request's tool declarations. The `model` argument is unused and kept only so
// the call sites read uniformly with the other request builders: normalization
// is model independent and lives in internal/util, shared with the WorkBuddy
// and Cline channels.
//
// The NoTools / no-declaration guard stays here rather than in the shared
// helper: Qoder distinguishes "no tools" (nil, so the gateway sees an empty
// array via the chatBody fallback below) from "tools present", while WorkBuddy
// always keeps a non-nil slice.
func normalizeToolDefinitions(req upstream.UpstreamRequest, model modelEntry) []interface{} {
	if req.NoTools || len(req.Tools) == 0 {
		return nil
	}
	return util.NormalizeToolDefinitions(req.Tools)
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
	return c.applyAuthHeadersBytes(req, creds, fields, requestID, modelKey, modelSource, []byte(body), signedPath)
}

func (c *Client) applyAuthHeadersBytes(req *http.Request, creds Credentials, fields RuntimeFields, requestID, modelKey, modelSource string, body []byte, signedPath string) error {
	unixSeconds := strconv.FormatInt(time.Now().Unix(), 10)
	authorization, err := buildCOSYAuthorization(requestID, fields.EncryptUserInfo, c.clientVersion, fields.Key, unixSeconds, body, signedPath)
	if err != nil {
		return err
	}
	// One request-owned backing array replaces a separate allocation per value.
	// Full slice expressions prevent Header.Add from overwriting its neighbour.
	if len(req.Header) == 0 {
		req.Header = make(http.Header, 32)
	}
	var values [32]string
	index := 0
	set := func(key, value string) {
		values[index] = value
		req.Header[textproto.CanonicalMIMEHeaderKey(key)] = values[index : index+1 : index+1]
		index++
	}
	set("Accept", "text/event-stream")
	set("Accept-Language", "*")
	set("Authorization", authorization)
	set("Cache-Control", "no-cache")
	set("Connection", "keep-alive")
	set("Content-Type", "application/json")
	set("Cosy-Business-Product", sceneBusinessProduct)
	set("Cosy-Business-Type", sceneBusinessType)
	set("Cosy-ClientType", sceneClientID)
	set("Cosy-Data-Policy", dataPolicyHeader(c.dataPolicyAgreed()))
	set("Cosy-Date", unixSeconds)
	set("Cosy-Key", fields.Key)
	set("Cosy-MachineId", c.machineID)
	set("Cosy-MachineOS", machineOS)
	set("Cosy-MachineToken", c.machineTokenOr(c.machineID))
	set("Cosy-MachineType", c.machineTypeOr(machineSceneType))
	if orgID := strings.TrimSpace(creds.OrgID); orgID != "" {
		set("Cosy-Organization-Id", orgID)
	}
	if tags := filterTags(creds.OrgTags); len(tags) > 0 {
		set("Cosy-Organization-Tags", strings.Join(tags, ","))
	}
	set("Cosy-Scene", sceneName)
	set("Cosy-User", strings.TrimSpace(creds.UID))
	set("Cosy-Version", c.clientVersion)
	set("Sec-Fetch-Mode", "cors")
	set("Traceparent", util.Traceparent(requestID))
	set("User-Agent", clientUserAgent)
	set("Login-Version", "v2")
	if key := strings.TrimSpace(modelKey); key != "" {
		set("X-Model-Key", key)
		// The source header is gated on the key, not on its own value: the CLI
		// sends it even when the source itself is empty.
		set("X-Model-Source", strings.TrimSpace(modelSource))
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
func (c *Client) attemptChat(ctx context.Context, url string, body []byte, model modelEntry, requestID string, fields RuntimeFields, creds Credentials, toolsEnabled bool, emit func(upstream.SSEMessage)) (result streamResult, attemptErr error) {
	reqCtx, cancel := util.WithDefaultTimeout(ctx, c.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return streamResult{}, &attemptStreamError{err: fmt.Errorf("build qoder request: %w", err)}
	}
	if err := c.applyAuthHeadersBytes(req, creds, fields, requestID, model.Key, model.Source, body, signPath(url)); err != nil {
		return streamResult{}, &attemptStreamError{err: err}
	}
	attempt := debug.BeginUpstream(ctx, req.Method, req.URL.String(), req.Header, body)
	var traceMetadata map[string]interface{}
	if debug.FromContext(ctx) != nil {
		traceMetadata = map[string]interface{}{"provider": "qoder", "model_key": model.Key, "model_source": model.Source, "host": req.URL.Host, "httpdns_ip": req.Header.Get("X-Qoder-Httpdns-Ip"), "body_bytes": len(body)}
	}
	traceCtx, latency := attempt.Trace(req.Context(), traceMetadata)
	if latency != nil {
		c.stateMu.RLock()
		if c.account != nil {
			latency.Set("account_id", c.account.ID)
		}
		c.stateMu.RUnlock()
		if transport, ok := c.stream.Transport.(*http.Transport); ok {
			proxy := "direct"
			if transport.Proxy != nil {
				if u, e := transport.Proxy(req); e == nil && u != nil {
					proxy = u.Scheme + "://" + u.Host
				}
			}
			latency.Set("proxy", proxy)
		}
		if raw, e := decodeBody(body); e == nil {
			var wire chatBody
			if json.Unmarshal(raw, &wire) == nil {
				latency.Set("parameters", wire.Parameters)
				latency.Set("messages_count", len(wire.Messages))
				latency.Set("tools_count", len(wire.Tools))
				latency.Set("conversation_fingerprint", fmt.Sprintf("%x", sha256.Sum256([]byte(wire.SessionID)))[:12])
			}
		}
	}
	req = req.WithContext(traceCtx)
	defer func() { latency.Finish(attemptErr) }()
	originalEmit := emit
	emit = func(m upstream.SSEMessage) {
		if m.Type == "model.text-delta" {
			latency.Mark("first_text_ms")
		}
		if m.Type == "model.reasoning-delta" {
			latency.Mark("first_reasoning_ms")
		}
		if m.Type == "model.tool-call" {
			latency.Mark("first_tool_ms")
		}
		if originalEmit != nil {
			originalEmit(m)
		}
	}
	resp, err := c.stream.Do(req)
	attempt.Response(resp, err)
	latency.Response(resp)
	if err != nil {
		// Only a connection-level hiccup is worth another attempt; a bad URL or
		// an untrusted certificate would fail identically every time.
		return streamResult{}, &attemptStreamError{err: fmt.Errorf("send qoder request: %w", err), retryable: IsTransientTransport(err)}
	}
	resp.Body = attempt.CaptureBody(resp.Body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return streamResult{}, classifyStatus(resp.StatusCode, resp.Header.Get("Retry-After"), raw)
	}

	result, err = consumeStreamObserved(resp.Body, toolsEnabled, emit, func() { latency.Mark("first_sse_ms") })
	for _, key := range []string{"firstTokenDuration", "totalDuration", "serverDuration"} {
		if v, ok := result.Usage[key]; ok {
			latency.Set("upstream_"+key, v)
		}
	}
	if err != nil {
		var target *attemptStreamError
		if errors.As(err, &target) {
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
		detail = util.Truncate(string(raw), 300)
	}
	wrapped := apiError(http.MethodPost, "chat", status, raw)

	if code == busyCode || sharedQueueRefusal(detail, string(raw)) {
		return &attemptStreamError{err: fmt.Errorf("%w: %v", ErrBusy, wrapped), busy: true, retryable: true, wait: busyWait(retryAfter, raw)}
	}
	// The daily request count is spent, and the gateway reports that under the
	// same 401/403 envelope it uses for a rejected credential. Reading it as an
	// authentication failure rotated the account's OAuth token and replayed the
	// request for a credential that was never the problem.
	if IsDailyCountExceeded(detail, string(raw)) {
		return &attemptStreamError{err: fmt.Errorf("%w: %s", ErrDailyCountExceeded, detail), retryable: true}
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

func retryAfterDelay(value string) time.Duration { return retryAfterDelayAt(value, time.Now()) }

func retryAfterDelayAt(value string, now time.Time) time.Duration {
	return httpclient.ParseRetryAfter(value, now, 30*time.Second)
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
