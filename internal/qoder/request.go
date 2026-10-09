package qoder

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"encoding/json"

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
