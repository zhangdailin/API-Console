package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	rtdebug "runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/adapter"
	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/logutil"
	"orchids-api/internal/middleware"
	"orchids-api/internal/pricing"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/tokencache"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

type responseWriterUnwrapper interface {
	Unwrap() http.ResponseWriter
}

func responseWriterSupportsFlush(w http.ResponseWriter) bool {
	for depth := 0; w != nil && depth < 32; depth++ {
		unwrapper, ok := w.(responseWriterUnwrapper)
		if !ok {
			_, supports := w.(http.Flusher)
			return supports
		}
		next := unwrapper.Unwrap()
		if next == nil || next == w {
			return false
		}
		w = next
	}
	return false
}

// ClientFactory creates an upstream client for a given account.
// Used to decouple provider-specific client construction from the handler.
type ClientFactory func(acc *store.Account, cfg *config.Config) UpstreamClient

type Handler struct {
	configMu      sync.RWMutex
	config        *config.Config
	client        UpstreamClient
	clientFactory ClientFactory
	clientCache   *accountClientCache
	loadBalancer  *loadbalancer.LoadBalancer
	connTracker   loadbalancer.ConnTracker
	tokenCache    tokencache.Cache
	promptCache   tokencache.PromptCache
	auditLogger   audit.Logger

	sessionStore SessionStore
	// Completed API requests update usage asynchronously. Coalescing by account
	// keeps this path at one worker instead of spawning a goroutine per request.
	statsOnce      sync.Once
	statsCloseOnce sync.Once
	statsMu        sync.Mutex
	statsPending   map[string]accountStatsDelta
	statsWake      chan struct{}
	statsStop      chan struct{}
	statsDone      chan struct{}
	statsClosed    bool
}

type UpstreamClient interface {
	SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error
}

type ClaudeRequest struct {
	MaxTokens           *int                   `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                   `json:"max_completion_tokens,omitempty"`
	Temperature         *float64               `json:"temperature,omitempty"`
	TopP                *float64               `json:"top_p,omitempty"`
	Stop                StopSequences          `json:"stop,omitempty"`
	StopSequences       []string               `json:"stop_sequences,omitempty"`
	Model               string                 `json:"model"`
	Messages            []prompt.Message       `json:"messages"`
	System              SystemItems            `json:"system"`
	Tools               []interface{}          `json:"tools"`
	ToolChoice          interface{}            `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool                  `json:"parallel_tool_calls,omitempty"`
	Stream              bool                   `json:"stream"`
	ConversationID      string                 `json:"conversation_id"`
	ConversationIDAlt   string                 `json:"conversationId"`
	Metadata            map[string]interface{} `json:"metadata"`
	// ReasoningEffort is the OpenAI-style effort hint. A catalog publishes models
	// as "<family>-<effort>", so a client that asks for the family name plus an
	// effort must have it resolved onto the catalog entry.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// OutputConfig and Thinking carry the Anthropic-side effort hints Claude
	// Code sends (output_config.effort, thinking.effort/budget_tokens). They
	// feed the same effort resolution as reasoning_effort.
	OutputConfig map[string]interface{} `json:"output_config,omitempty"`
	Thinking     map[string]interface{} `json:"thinking,omitempty"`
}

type toolCall struct {
	id    string
	name  string
	input string
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
	Content          interface{}               `json:"content"`
	ReasoningContent string                    `json:"reasoning_content,omitempty"`
	ToolCalls        []openAINonStreamToolCall `json:"tool_calls,omitempty"`
}

type openAINonStreamChoice struct {
	Index        int                    `json:"index"`
	Message      openAINonStreamMessage `json:"message"`
	FinishReason *string                `json:"finish_reason"`
}

type openAINonStreamUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openAINonStreamResponse struct {
	ID      string                  `json:"id"`
	Object  string                  `json:"object"`
	Created int64                   `json:"created"`
	Model   string                  `json:"model"`
	Choices []openAINonStreamChoice `json:"choices"`
	Usage   openAINonStreamUsage    `json:"usage"`
}

const keepAliveInterval = 15 * time.Second
const maxRequestBytes = 50 * 1024 * 1024 // 50MB

// sessionTTL is how long a conversation binding survives an idle gap. A missing
// or non-positive setting keeps the historical half hour; the configured value
// is what a deployment raises so a long session is not detached mid-way.
func sessionTTL(cfg *config.Config) time.Duration {
	const fallback = 30 * time.Minute
	if cfg == nil || cfg.SessionTTLMinutes <= 0 {
		return fallback
	}
	return time.Duration(cfg.SessionTTLMinutes) * time.Minute
}

func NewWithLoadBalancer(cfg *config.Config, lb *loadbalancer.LoadBalancer) *Handler {
	h := &Handler{
		config:       cfg,
		loadBalancer: lb,
		connTracker:  loadbalancer.NewMemoryConnTracker(),
		clientCache:  newAccountClientCache(),
		sessionStore: NewMemorySessionStore(sessionTTL(cfg), 1024),
		auditLogger:  audit.NewNopLogger(),
	}
	h.clientCache.SetConfig(cfg)
	// The cache re-reads an account when it is told the account changed, so the
	// decision "is this client still valid?" uses the state that was persisted
	// rather than the event alone.
	h.clientCache.SetAccountResolver(func(id int64) *store.Account {
		if lb == nil || lb.Store == nil || id == 0 {
			return nil
		}
		account, err := lb.Store.GetAccount(context.Background(), id)
		if err != nil {
			return nil
		}
		return account
	})

	return h
}

// SetConnTracker makes selection and reservation use the deployment-wide
// tracker. In Redis mode this keeps per-account WorkBuddy limits correct across
// every handler instance instead of maintaining a disconnected local count.
func (h *Handler) SetConnTracker(tracker loadbalancer.ConnTracker) {
	if h != nil && tracker != nil {
		h.connTracker = tracker
	}
}

// SetConfig atomically changes the immutable config snapshot used by future
// requests. Requests already in progress keep their existing snapshot.
func (h *Handler) SetConfig(cfg *config.Config) {
	if h == nil || cfg == nil {
		return
	}
	h.configMu.Lock()
	h.config = cfg
	h.configMu.Unlock()
	if h.clientCache != nil {
		h.clientCache.SetConfig(cfg)
	}
}

func (h *Handler) configSnapshot() *config.Config {
	if h == nil {
		return nil
	}
	h.configMu.RLock()
	cfg := h.config
	h.configMu.RUnlock()
	return cfg
}

func (h *Handler) SetTokenCache(cache tokencache.Cache) {
	h.tokenCache = cache
}

func (h *Handler) SetPromptCache(cache tokencache.PromptCache) {
	h.promptCache = cache
}

// SetSessionStore replaces the default in-memory session store.
func (h *Handler) SetSessionStore(ss SessionStore) {
	h.sessionStore = ss
}

// SetAuditLogger replaces the default nop audit logger.
func (h *Handler) SetAuditLogger(al audit.Logger) {
	h.auditLogger = al
}

// SetClientFactory sets the factory used by selectAccount to create provider-specific clients.
func (h *Handler) SetClientFactory(f ClientFactory) {
	h.clientFactory = f
}

func (h *Handler) computeRequestHash(r *http.Request, body []byte) string {
	hasher := sha256.New()
	hasher.Write([]byte(r.URL.Path))
	hasher.Write([]byte{0})
	for _, identity := range []string{middleware.APIKeyFingerprint(r.Context()), r.Header.Get("Authorization"), r.Header.Get("X-API-Key")} {
		hasher.Write([]byte(identity))
		hasher.Write([]byte{0})
	}
	hasher.Write([]byte{0})
	hasher.Write(body)
	return hex.EncodeToString(hasher.Sum(nil))
}

func mapStopReasonToOpenAIFinishReason(stopReason string) *string {
	switch strings.TrimSpace(stopReason) {
	case "", "end_turn", "stop":
		reason := "stop"
		return &reason
	case "tool_use":
		reason := "tool_calls"
		return &reason
	case "max_tokens":
		reason := "length"
		return &reason
	case "refusal":
		reason := "content_filter"
		return &reason
	default:
		reason := stopReason
		return &reason
	}
}

func buildOpenAINonStreamResponse(sh *streamHandler, model string, stopReason string) openAINonStreamResponse {
	textParts := make([]string, 0, len(sh.contentBlocks))
	reasoningParts := make([]string, 0, len(sh.contentBlocks))
	toolCalls := make([]openAINonStreamToolCall, 0)

	for i := range sh.contentBlocks {
		blockType, _ := sh.contentBlocks[i]["type"].(string)
		switch blockType {
		case "thinking":
			if builder := builderAt(sh.thinkingBlockBuilders, i); builder != nil {
				if reasoning := builder.String(); reasoning != "" {
					reasoningParts = append(reasoningParts, reasoning)
					continue
				}
			}
			if reasoning, ok := sh.contentBlocks[i]["thinking"].(string); ok && reasoning != "" {
				reasoningParts = append(reasoningParts, reasoning)
			}
		case "text":
			if builder := builderAt(sh.textBlockBuilders, i); builder != nil {
				if text := builder.String(); text != "" {
					textParts = append(textParts, text)
					continue
				}
			}
			if text, ok := sh.contentBlocks[i]["text"].(string); ok && text != "" {
				textParts = append(textParts, text)
			}
		case "tool_use":
			call := openAINonStreamToolCall{
				Type: "function",
			}
			if id, ok := sh.contentBlocks[i]["id"].(string); ok {
				call.ID = id
			}
			if name, ok := sh.contentBlocks[i]["name"].(string); ok {
				call.Function.Name = name
			}
			switch input := sh.contentBlocks[i]["input"].(type) {
			case string:
				call.Function.Arguments = input
			case nil:
				call.Function.Arguments = "{}"
			default:
				raw, err := json.Marshal(input)
				if err != nil {
					call.Function.Arguments = "{}"
				} else {
					call.Function.Arguments = string(raw)
				}
			}
			toolCalls = append(toolCalls, call)
		}
	}

	content := strings.Join(textParts, "")
	if strings.TrimSpace(content) == "" && len(toolCalls) > 0 {
		content = ""
	}

	message := openAINonStreamMessage{
		Role:             "assistant",
		Content:          content,
		ReasoningContent: strings.Join(reasoningParts, ""),
	}
	if len(toolCalls) > 0 {
		message.ToolCalls = toolCalls
	}

	return openAINonStreamResponse{
		ID:      sh.msgID,
		Object:  "chat.completion",
		Created: sh.startTime.Unix(),
		Model:   model,
		Choices: []openAINonStreamChoice{{
			Index:        0,
			Message:      message,
			FinishReason: mapStopReasonToOpenAIFinishReason(stopReason),
		}},
		Usage: openAINonStreamUsage{
			PromptTokens:     sh.inputTokens,
			CompletionTokens: sh.outputTokens,
			TotalTokens:      sh.inputTokens + sh.outputTokens,
		},
	}
}

func shortRequestTrace(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func (h *Handler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	streamingStarted := false

	defer func() {
		if err := recover(); err != nil {
			stack := string(rtdebug.Stack())
			slog.Error("Panic in HandleMessages", "error", err, "stack", stack)
			if streamingStarted {
				// Headers already sent — write an SSE error event instead of HTTP error
				// Pre-compiled zero-allocation string
				fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"message\":\"Internal Server Error\"}}\n\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			} else {
				apperrors.New("server_error", "Internal Server Error", http.StatusInternalServerError).WriteResponse(w)
			}
		}
	}()

	if r.Method != http.MethodPost {
		apperrors.New("invalid_request_error", "Method not allowed", http.StatusMethodNotAllowed).WriteResponse(w)
		return
	}

	var req ClaudeRequest
	if maxRequestBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	}
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		if maxRequestBytes > 0 {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				apperrors.New("invalid_request_error", "Request body too large", http.StatusRequestEntityTooLarge).WriteResponse(w)
				return
			}
		}
		apperrors.New("invalid_request_error", "Invalid request body", http.StatusBadRequest).WriteResponse(w)
		return
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		apperrors.New("invalid_request_error", "Invalid request body", http.StatusBadRequest).WriteResponse(w)
		return
	}
	if !middleware.APIKeyAllowsModel(r.Context(), req.Model) {
		apperrors.New("permission_error", "API key is not allowed to use model "+strings.TrimSpace(req.Model), http.StatusForbidden).WriteResponse(w)
		return
	}
	responseFormat := adapter.DetectResponseFormat(r.URL.Path)

	// 初始化调试日志
	cfg := h.configSnapshot()
	logger := debug.NewForContext(r.Context(), cfg.DebugEnabled, cfg.DebugLogSSE)
	defer logger.Close()
	verboseDiagnostics := logutil.VerboseDiagnosticsEnabled()

	// 1. 记录进入的 Claude 请求
	logger.LogIncomingRequest(req)

	reqHash := h.computeRequestHash(r, bodyBytes)
	traceID := shortRequestTrace(reqHash)
	if verboseDiagnostics {
		slog.Debug("Request fingerprint", "trace_id", traceID, "hash", reqHash, "path", r.URL.Path, "content_length", len(bodyBytes))
	}

	// ...
	if ok, command := isCommandPrefixRequest(req); ok {
		if verboseDiagnostics {
			slog.Debug("Handling command prefix request", "command", command)
		}
		prefix := detectCommandPrefix(command)
		logger.LogEarlyExit("command_prefix", map[string]interface{}{
			"command": command,
			"prefix":  prefix,
		})
		writeCommandPrefixResponse(w, req, responseFormat, prefix, startTime, logger)
		return
	}

	if isTopicClassifierRequest(req) {
		if verboseDiagnostics {
			slog.Debug("Handling topic classifier request locally")
		}
		logger.LogEarlyExit("topic_classifier", map[string]interface{}{
			"mode": "local",
		})
		writeTopicClassifierResponse(w, req, responseFormat, startTime, logger)
		return
	}

	if isTitleGenerationRequest(req) {
		title := generateTopicTitle(extractUserText(req.Messages))
		if verboseDiagnostics {
			slog.Debug("Handling title generation request locally", "title", title)
		}
		logger.LogEarlyExit("title_generation", map[string]interface{}{
			"mode":  "local",
			"title": title,
		})
		writeTitleGenerationResponse(w, req, responseFormat, startTime, logger)
		return
	}

	cacheStrategy := cfg.CacheStrategy
	if cacheStrategy != "" && cacheStrategy != "none" {
		applyCacheStrategy(&req, cacheStrategy)
	}

	// Debug: log all headers
	if verboseDiagnostics {
		for k, v := range r.Header {
			slog.Debug("Incoming header V2 CHECK", "key", k, "value", v)
		}
	}

	// Context and Conversation Key
	conversationKey := conversationKeyForRequest(r, req)
	if verboseDiagnostics {
		slog.Debug("Request dispatch initialized", "trace_id", traceID, "path", r.URL.Path, "conversation_id", conversationKey, "model", req.Model, "stream", req.Stream)
	}

	// The path names a channel only on a channel-prefixed route; on the unified
	// prefix the model does. A model that ends in an effort word is only treated
	// as such when the path did not already pin a channel (".../models/gpt-5-x-low"
	// under a channel prefix is a real row, not a family plus an effort).
	forcedChannel := channelFromPath(r.URL.Path)
	effort := requestReasoningEffort(req)
	if forcedChannel == "" {
		if _, level := splitEffortVariantSuffix(normalizeRequestedModelID(req.Model)); level != "" {
			effort = ""
		}
	}
	req.Model = h.resolveEffortModelVariant(r.Context(), req.Model, effort, forcedChannel)
	validatedModel, err := h.validateModelAvailability(r.Context(), req.Model, forcedChannel)
	if err != nil {
		apperrors.New("invalid_request_error", err.Error(), http.StatusBadRequest).WriteResponse(w)
		return
	}
	targetChannel := strings.TrimSpace(forcedChannel)
	if targetChannel == "" && validatedModel != nil {
		targetChannel = strings.TrimSpace(validatedModel.Channel)
	}
	// The gateway no longer models a working directory at all. It used to extract
	// one from headers/system/messages, remember it per conversation, drop the
	// upstream session whenever it changed, answer "当前工作目录" locally without
	// calling upstream, and rebase foreign tool paths onto it. Every one of those
	// behaviours is gone: the request that reached upstream is now the request the
	// caller wrote.
	if isSuggestionMode(req.Messages) {
		suggestion := buildLocalSuggestion(req.Messages)
		if verboseDiagnostics {
			slog.Debug("Handling suggestion mode request locally", "suggestion", suggestion)
		}
		logger.LogEarlyExit("suggestion_mode", map[string]interface{}{
			"mode":       "local",
			"suggestion": suggestion,
		})
		writeSuggestionModeResponse(w, req, responseFormat, startTime, logger)
		return
	}

	preSelectWorkBuddyRequest := strings.EqualFold(targetChannel, "workbuddy")
	// Qoder forwards raw OpenAI-style messages like WorkBuddy does, so it
	// belongs to the same passthrough family: the request verbatim as the caller
	// sent it.
	preSelectQoderRequest := strings.EqualFold(targetChannel, "qoder")
	// Cline is the same kind of passthrough: its endpoint is OpenAI-shaped and
	// the client's messages are forwarded verbatim.
	preSelectClineRequest := strings.EqualFold(targetChannel, "cline")
	preSelectPassthroughRequest := preSelectWorkBuddyRequest || preSelectQoderRequest || preSelectClineRequest
	suggestionMode := isSuggestionMode(req.Messages)
	noThinking := suggestionMode || cfg.SuppressThinking
	gateNoTools := false
	toolGateReasons := make([]string, 0, 2)
	toolGateMessage := ""
	if toolChoiceDisablesTools(req.ToolChoice) {
		gateNoTools = true
		toolGateReasons = append(toolGateReasons, "tool_choice_none")
		toolGateMessage = buildToolGateMessage(req.Messages, suggestionMode)
	}
	if suggestionMode {
		gateNoTools = true
		toolGateReasons = append(toolGateReasons, "suggestion_mode")
		toolGateMessage = buildToolGateMessage(req.Messages, true)
	}
	if lastUserIsToolResultFollowup(req.Messages) {
		if preSelectPassthroughRequest {
			if verboseDiagnostics {
				slog.Debug("tool_gate: keeping tools for passthrough tool_result follow-up")
			}
		} else {
			gateNoTools = true
			toolGateReasons = append(toolGateReasons, "tool_result_followup")
			toolGateMessage = buildToolGateMessage(req.Messages, suggestionMode)
			if verboseDiagnostics {
				slog.Debug("tool_gate: disabled tools for tool_result-only follow-up")
			}
		}
	}
	effectiveTools := req.Tools
	if gateNoTools {
		effectiveTools = nil
		if verboseDiagnostics {
			slog.Debug("tool_gate: disabled tools", "reasons", toolGateReasons)
		}
	}
	chatSessionID := ""

	// 选择账号 (Initial Selection)
	failedAccountIDs := []int64{}
	failedAccountSet := make(map[int64]struct{})

	apiClient, currentAccount, releaseClient, trackedAccountID, err := h.acquireReservedAccountSelection(r.Context(), targetChannel, forcedChannel != "", failedAccountIDs, accountSelectionOptions{
		ModelID: strings.TrimSpace(req.Model),
	})
	// The client is held for the whole request: a credential change during it
	// retires the client and closes it here, after the request finished.
	defer func() { releaseClient() }()
	defer func() { h.releaseTrackedAccount(trackedAccountID) }()
	if err != nil {
		slog.Error("selectAccount failed", "error", err, "channel", targetChannel)
		logger.LogEarlyExit("select_account_failed", map[string]interface{}{
			"error":   err.Error(),
			"model":   req.Model,
			"channel": targetChannel,
		})
		// The pool's note says why it is empty (cooling down for this model, rate
		// limited, allowance spent, all busy). It stays in the log; the client gets
		// the shared answer for that cause, with the status the cause implies — a
		// capacity problem is a retryable 429, not a 503 server fault.
		writePoolExhaustion(w, classifyPoolExhaustion(err, err.Error()))
		return
	}
	if verboseDiagnostics {
		slog.Debug("Checkpoint: selectAccount success")
	}

	isWorkBuddyRequest := preSelectWorkBuddyRequest
	if currentAccount != nil && strings.EqualFold(currentAccount.AccountType, "workbuddy") {
		isWorkBuddyRequest = true
	}
	isQoderRequest := preSelectQoderRequest
	if currentAccount != nil && strings.EqualFold(currentAccount.AccountType, "qoder") {
		isQoderRequest = true
	}
	isClineRequest := preSelectClineRequest
	if currentAccount != nil && strings.EqualFold(currentAccount.AccountType, "cline") {
		isClineRequest = true
	}
	isPassthroughRequest := isWorkBuddyRequest || isQoderRequest || isClineRequest
	if isPassthroughRequest {
		channel := ""
		switch {
		case isWorkBuddyRequest:
			channel = "workbuddy"
		case isQoderRequest:
			channel = "qoder"
		case isClineRequest:
			channel = "cline"
		}
		// Passthrough channels do not trim history/tool results.
		if verboseDiagnostics {
			slog.Debug("Checkpoint: passthrough, skip context trimming", "channel", channel)
		}
	}
	if verboseDiagnostics {
		slog.Debug("Checkpoint: message processing done")
	}

	// The account slot was atomically reserved with selection. Keeping the
	// reservation from this point through the complete SSE prevents concurrent
	// requests from racing past a per-account limit before either increments it.

	// 构建 prompt（V2 Markdown 格式）
	startBuild := time.Now()
	if verboseDiagnostics {
		slog.Debug("Starting prompt build...", "conversation_id", conversationKey)
	}
	// 映射模型（用于上游请求与提示一致）
	mappedModel := mapModel(req.Model)
	if isWorkBuddyRequest || isQoderRequest || isClineRequest {
		mappedModel = strings.TrimSpace(req.Model)
	}

	builtPrompt := strings.TrimSpace(extractUserText(req.Messages))
	if builtPrompt == "" {
		switch {
		case isWorkBuddyRequest:
			builtPrompt = "workbuddy request"
		case isQoderRequest:
			builtPrompt = "qoder request"
		case isClineRequest:
			builtPrompt = "cline request"
		default:
			builtPrompt = "request"
		}
	}
	buildDuration := time.Since(startBuild)
	if verboseDiagnostics {
		slog.Debug("Prompt build completed", "duration", buildDuration)
		slog.Debug("[Performance] BuildPromptAndHistory", "duration", buildDuration)
	}

	if verboseDiagnostics {
		slog.Debug("Model mapping", "original", req.Model, "mapped", mappedModel)
	}

	isStream := req.Stream

	if isStream {
		// Check the complete middleware chain before committing SSE headers. A
		// wrapper may expose Flush while its underlying writer cannot actually
		// flush, so unwrap to the real server writer before accepting the stream.
		if !responseWriterSupportsFlush(w) {
			apperrors.New("api_error", "Streaming not supported by underlying connection", http.StatusInternalServerError).WriteResponse(w)
			return
		}
		// 设置 SSE 响应头
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		streamingStarted = true
	} else {
		w.Header().Set("Content-Type", "application/json")
	}

	// 状态管理
	// msgID is now managed by streamHandler

	upstreamMessages := append([]prompt.Message(nil), req.Messages...)

	if gateNoTools {
		builtPrompt = injectToolGate(builtPrompt, toolGateMessage)
	}

	// 2. 记录转换后的 prompt
	if verboseDiagnostics {
		slog.Debug("Checkpoint: LogConvertedPrompt")
	}
	logger.LogConvertedPrompt(builtPrompt)

	breakdown := estimateInputTokenBreakdown(builtPrompt, effectiveTools)
	breakdownProfile := ""
	switch {
	case isWorkBuddyRequest:
		breakdownProfile = "workbuddy"
	case isQoderRequest:
		breakdownProfile = "qoder"
	case isClineRequest:
		breakdownProfile = "cline"
	}
	if verboseDiagnostics {
		slog.Debug(
			"Input token breakdown (estimated)",
			"prompt_profile", breakdownProfile,
			"base_prompt_tokens", breakdown.BasePromptTokens,
			"system_context_tokens", breakdown.SystemContextTokens,
			"history_tokens", breakdown.HistoryTokens,
			"tools_tokens", breakdown.ToolsTokens,
			"estimated_total_input_tokens", breakdown.Total,
		)
	}
	logger.LogInputTokenBreakdown(
		breakdownProfile,
		breakdown.BasePromptTokens,
		breakdown.SystemContextTokens,
		breakdown.HistoryTokens,
		breakdown.ToolsTokens,
		breakdown.Total,
	)

	// Token 计数（用于前置 usage 展示）
	inputTokens := breakdown.Total
	if inputTokens <= 0 {
		inputTokens = h.estimateInputTokens(r.Context(), req.Model, builtPrompt)
	}

	if cfg.EnableTokenCache && h.promptCache != nil {
		sysText := ""
		if len(req.System) > 0 {
			if sysBytes, err := json.Marshal(req.System); err == nil {
				sysText = string(sysBytes)
			}
		}
		toolsText := ""
		if len(effectiveTools) > 0 {
			if toolsBytes, err := json.Marshal(effectiveTools); err == nil {
				toolsText = string(toolsBytes)
			}
		}

		cacheReadTokens, _ := h.promptCache.CheckPromptCache(
			cfg.TokenCacheStrategy,
			breakdown.SystemContextTokens,
			breakdown.ToolsTokens,
			sysText,
			toolsText,
		)
		// Subtract cacheReadTokens from the base inputTokens
		// if simulating prompt caching billing behavior
		if inputTokens >= cacheReadTokens {
			inputTokens -= cacheReadTokens
		}
	}

	sh := newStreamHandler(
		cfg, w, logger, noThinking, isStream, responseFormat,
	)
	allowedToolNames := []string(nil)
	allowedToolNames = validationAllowedToolNames(effectiveTools, req.Tools, false)
	sh.setAllowedToolNames(allowedToolNames)
	if preSelectQoderRequest {
		sh.setSurfaceToolRejects(true)
	}
	if len(req.Tools) > 0 {
		sh.setClientTools(req.Tools)
	} else if len(effectiveTools) > 0 {
		sh.setClientTools(effectiveTools)
	}
	sh.setDisallowToolCalls(gateNoTools)
	sh.setEmptyOutputFallback(successfulFileMutationToolResultFallback(upstreamMessages))
	sh.setUsageTokens(inputTokens, -1) // Correctly initialize input tokens
	// Capture the server-issued conversation id so the next turn of the same
	// client session resumes the same upstream conversation.
	sh.onConversationID = func(id string) {
		id = strings.TrimSpace(id)
		if conversationKey != "" {
			h.sessionStore.SetConvID(r.Context(), conversationKey, id)
			if currentAccount != nil {
				h.sessionStore.SetAccountID(r.Context(), conversationKey, currentAccount.ID)
			}
			h.sessionStore.Touch(r.Context(), conversationKey)
		}
		if verboseDiagnostics {
			slog.Debug("conversationID captured", "key", conversationKey, "id", id)
		}
	}
	defer sh.release()

	// The opening frame is deliberately NOT written here. Writing it before the
	// upstream is called committed 200 and a role chunk to every streaming client,
	// so a shared queue refusal arriving afterwards could only be reported in band
	// -- which clients surface as a truncated stream instead of a retryable 429.
	// Keep-alive ticks also wait until actual content has opened the stream, so
	// a queue refusal after 15 seconds can still carry a real HTTP status.
	sh.pendingModel = req.Model

	if verboseDiagnostics {
		slog.Debug("New request received")
	}

	// KeepAlive
	//
	// The watchdog captures its cancellation channel before the goroutine starts.
	// Reading it inside the loop would race with the request value reassigned
	// later in this handler (`r = r.WithContext(...)`), and a cancellation that
	// arrived during that window could be missed — leaving the watchdog to
	// outlive the client.
	keepAliveDone := r.Context().Done()
	var keepAliveStop chan struct{}
	if isStream {
		keepAliveStop = make(chan struct{})
		defer close(keepAliveStop)
		ticker := time.NewTicker(keepAliveInterval)
		go func() {
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					sh.mu.Lock()
					done := sh.hasReturn
					sh.mu.Unlock()
					if done {
						return
					}
					sh.writeKeepAlive()
				case <-keepAliveStop:
					return
				case <-keepAliveDone:
					return
				}
			}
		}()
	}

	// Main execution
	run := func() {
		// 复用上游返回的 conversationID，保持会话连续性
		if chatSessionID == "" {
			chatSessionID = "chat_" + randomSessionID()
		}
		maxRetries := cfg.MaxRetries
		if maxRetries < 0 {
			maxRetries = 0
		}
		retryDelay := time.Duration(cfg.RetryDelay) * time.Millisecond
		retriesRemaining := maxRetries

		// Publish the model this request resolved to, so the per-minute
		// aggregation can attribute the outcome to a model rather than only to a
		// channel. The middleware cannot read the body itself.
		r = r.WithContext(middleware.WithRequestModel(r.Context(), mappedModel))

		payloadMessages := upstreamMessages
		payloadSystem := req.System

		upstreamReq := upstream.UpstreamRequest{
			MaxTokens:         req.outputTokenLimit(),
			Temperature:       req.Temperature,
			TopP:              req.TopP,
			Stop:              req.stopSequences(),
			Prompt:            builtPrompt,
			Model:             mappedModel,
			Messages:          payloadMessages,
			System:            payloadSystem,
			Tools:             effectiveTools,
			ToolChoice:        req.ToolChoice,
			ParallelToolCalls: req.ParallelToolCalls,
			NoTools:           gateNoTools,
			ReasoningEffort:   effort,
			RequestID:         workBuddyConversationRequestID(r),
			ConversationID:    explicitConversationID(r, req),
			TraceID:           middleware.GetTraceID(r.Context()),
			ChatSessionID:     chatSessionID,
		}
		primaryHandler := sh.handleMessage
		var attempt int
		for {
			if returned, _ := sh.terminalState(); returned {
				return
			}
			sh.resetRoundState()
			var err error
			upstreamReq.Attempt = attempt + 1
			accountID := int64(0)
			accountType := ""
			accountName := ""
			if currentAccount != nil {
				accountID = currentAccount.ID
				accountType = currentAccount.AccountType
				accountName = currentAccount.Name
			}
			if verboseDiagnostics {
				slog.Debug(
					"Calling upstream client",
					"trace_id", traceID,
					"attempt", upstreamReq.Attempt,
					"max_attempts", maxRetries+1,
					"channel", targetChannel,
					"model", mappedModel,
					"conversation_id", conversationKey,
					"chat_session_id", chatSessionID,
					"account_id", accountID,
					"account_type", accountType,
					"account_name", accountName,
				)
			}

			if verboseDiagnostics {
				slog.Debug("Using SendRequestWithPayload")
			}
			err = apiClient.SendRequestWithPayload(r.Context(), upstreamReq, primaryHandler, logger)
			attemptAccountID := int64(0)
			if currentAccount != nil {
				attemptAccountID = currentAccount.ID
			}
			middleware.RecordUpstreamAttempt(r.Context(), attemptAccountID, err != nil)
			if verboseDiagnostics {
				slog.Debug("Upstream client returned", "trace_id", traceID, "attempt", upstreamReq.Attempt, "error", err)
			}

			if err == nil {
				sh.forceFinishIfMissing()
				if verboseDiagnostics {
					slog.Debug("Upstream attempt completed", "trace_id", traceID, "attempt", upstreamReq.Attempt)
				}
				break
			}
			// A provider may emit its authoritative finish frame and then observe a
			// transport cleanup error. Never reset terminal state and append a second
			// response in that case.
			if returned, failed := sh.terminalState(); returned {
				if failed {
					return
				}
				slog.Warn("Ignoring upstream error after terminal response", "trace_id", traceID, "attempt", upstreamReq.Attempt, "error", err)
				break
			}
			errStr := err.Error()
			errClass := apperrors.ClassifyUpstreamError(errStr)
			if sh.hasAnyOutput() {
				slog.Warn("Upstream failed after partial output, skip retry to avoid duplicated token billing", "trace_id", traceID, "attempt", upstreamReq.Attempt, "error", err)
				// Partial content is not a successful completion. Streaming responses
				// have already committed 200, so report the terminal failure in band;
				// non-streaming responses have committed nothing and can still return
				// the correct HTTP error without leaking the partial draft.
				sh.reportRequestFailure("Reporting upstream failure after partial output",
					errClass.Category, apperrors.PublicMessage(errStr))
				return
			}

			// Check for non-retriable errors
			slog.Error("Request error", "trace_id", traceID, "attempt", upstreamReq.Attempt, "error", err, "category", errClass.Category, "retryable", errClass.Retryable)
			// One decision for both questions this error raises: whether the
			// account keeps its place in the pool, and whether the request may be
			// retried. The scheduler reads the same policy, so a failure cannot be
			// "cooling down" for one entrance and "retryable" for the other.
			verdict := accountpolicy.Classify(currentAccount, err, req.Model)
			// 标记账号状态（auth 类错误始终标记，无论是否可重试）
			if currentAccount != nil && h.loadBalancer != nil && h.loadBalancer.Store != nil {
				if verdict.Scope == accountpolicy.ScopeModel && verdict.Model != "" && verdict.Cooldown > 0 {
					// WorkBuddy code 6004 is a model-frequency limit. Persist only
					// that model's cooldown; applying an empty account status here
					// would either be skipped or accidentally clear unrelated state.
					store.RecordModelCooldown(currentAccount, verdict.Model, time.Now().Add(verdict.Cooldown))
					if persistErr := h.loadBalancer.Store.UpdateAccount(r.Context(), currentAccount); persistErr != nil {
						slog.Warn("persist model cooldown failed", "account_id", currentAccount.ID, "model", verdict.Model, "error", persistErr)
					}
				} else if verdict.Status != "" {
					if verboseDiagnostics {
						slog.Debug("标记账号状态", "account_id", currentAccount.ID, "status", verdict.Status, "scope", string(verdict.Scope), "category", errClass.Category)
					}
					// Apply keeps the status and its operator-facing reason
					// together, so the account table can explain the cooldown.
					verdict.Apply(currentAccount)
					h.loadBalancer.PersistAppliedAccountStatus(r.Context(), currentAccount, "账号策略判定: "+verdict.Status)
				}
			}

			if !verdict.Retryable {
				slog.Error("Aborting retries for non-retriable error", "error", err, "category", errClass.Category)
				// A failure before any output is a failure, not an answer. The
				// raw upstream text goes to the log; the client gets the category.
				if errClass.Category == "canceled" {
					if r.Context().Err() != nil {
						sh.finishResponse("end_turn")
						return
					}
					sh.reportRequestFailure("Reporting unexpected upstream cancellation", "server", "Upstream request was canceled unexpectedly")
					return
				}
				sh.reportRequestFailure("Reporting non-retriable upstream failure",
					errClass.Category, apperrors.PublicMessage(errStr))
				return
			}

			// Shared queues may clear later within the advertised retry window.
			// Use the bounded retry budget on the same account rather than handing
			// every caller an early 429 after a single short probe. Once exhausted,
			// the uncommitted response below still returns an honest HTTP 429.

			if r.Context().Err() != nil {
				sh.finishResponse("end_turn")
				return
			}
			if retriesRemaining <= 0 {
				if currentAccount != nil && h.loadBalancer != nil {
					slog.Error("Account request failed, max retries reached", "account", currentAccount.Name)
				}
				// Same rule as the non-retriable branch above: with nothing sent yet
				// this is a gateway failure, and the client sees it as one.
				sh.reportRequestFailure("Reporting that retries are exhausted",
					errClass.Category, apperrors.PublicMessage(errStr))
				return
			}
			retriesRemaining--
			slog.Warn(
				"Retrying upstream request without prior output",
				"trace_id", traceID,
				"attempt", upstreamReq.Attempt,
				"category", errClass.Category,
				"switch_account", errClass.SwitchAccount,
				"retries_remaining", retriesRemaining,
			)
			if errClass.SwitchAccount && currentAccount != nil && h.loadBalancer != nil {
				prevClient := apiClient
				prevAccount := currentAccount
				if _, ok := failedAccountSet[currentAccount.ID]; !ok {
					failedAccountSet[currentAccount.ID] = struct{}{}
					failedAccountIDs = append(failedAccountIDs, currentAccount.ID)
				}
				slog.Warn("Account request failed, switching account", "account", currentAccount.Name, "unsuccessful_attempts", len(failedAccountIDs))

				// 释放旧账号的连接计数
				if trackedAccountID != 0 {
					h.releaseTrackedAccount(trackedAccountID)
					trackedAccountID = 0
				}

				nextClient, nextAccount, releaseNext, nextTrackedAccountID, retryErr := h.acquireReservedAccountSelection(r.Context(), targetChannel, forcedChannel != "", failedAccountIDs, accountSelectionOptions{
					ModelID: upstreamReq.Model,
				})
				if retryErr == nil {
					previousRelease := releaseClient
					apiClient = nextClient
					currentAccount = nextAccount
					releaseClient = releaseNext
					trackedAccountID = nextTrackedAccountID
					previousRelease()
					if verboseDiagnostics {
						if currentAccount != nil {
							slog.Debug("Switched to account", "account", currentAccount.Name)
						} else {
							slog.Debug("Switched to default upstream config")
						}
					}
				} else {
					if shouldRetryCurrentAccountWhenNoAlternative(errClass.Category) && prevAccount != nil {
						reacquiredID, acquired := h.tryAcquireTrackedAccount(prevAccount)
						if !acquired {
							slog.Error("No account concurrency slot available for retry", "account_id", prevAccount.ID, "category", errClass.Category)
							sh.InjectNoAvailableAccountError(errStr, retryErr)
							sh.finishResponse("end_turn")
							return
						}
						apiClient = prevClient
						currentAccount = prevAccount
						trackedAccountID = reacquiredID
						slog.Warn(
							"No alternate accounts available; retrying current account",
							"trace_id", traceID,
							"attempt", upstreamReq.Attempt,
							"account_id", currentAccount.ID,
							"category", errClass.Category,
							"retry_error", retryErr,
						)
					} else {
						slog.Error("No more accounts available", "error", retryErr)
						sh.InjectNoAvailableAccountError(errStr, retryErr)
						sh.finishResponse("end_turn")
						return
					}
				}
			}
			retryDelayForAttempt := computeRetryDelay(retryDelay, attempt+1, errClass.Category)
			if hinted := upstreamRetryAfter(err); hinted > retryDelayForAttempt {
				retryDelayForAttempt = hinted
			}
			if retryDelayForAttempt > 0 && isSharedUpstreamRefusalClass(errClass) {
				// Qoder preserves the provider-normalized hint. Other channels
				// keep their existing early-probe policy; positive jitter never
				// moves a retry before the chosen wait.
				retryDelayForAttempt = sharedRefusalWaitForChannel(retryDelayForAttempt, attempt+1, targetChannel)
				retryDelayForAttempt += sharedRefusalJitter(retryDelayForAttempt)
			}
			if retryDelayForAttempt > 0 && !util.SleepWithContext(r.Context(), retryDelayForAttempt) {
				sh.finishResponse("end_turn")
				return
			}
			attempt++
		}
	}

	run()

	// 确保有最终响应
	if !sh.hasReturn {
		sh.finishResponse("end_turn")
	}
	if !isStream && !sh.requestFailed {
		stopReason := sh.finalStopReason
		if stopReason == "" {
			stopReason = "end_turn"
		}

		for i := range sh.contentBlocks {
			blockType, _ := sh.contentBlocks[i]["type"].(string)
			switch blockType {
			case "text":
				if builder := builderAt(sh.textBlockBuilders, i); builder != nil {
					sh.contentBlocks[i]["text"] = builder.String()
				} else if _, ok := sh.contentBlocks[i]["text"]; !ok {
					sh.contentBlocks[i]["text"] = ""
				}
			case "thinking":
				if builder := builderAt(sh.thinkingBlockBuilders, i); builder != nil {
					sh.contentBlocks[i]["thinking"] = builder.String()
				} else if _, ok := sh.contentBlocks[i]["thinking"]; !ok {
					sh.contentBlocks[i]["thinking"] = ""
				}
			}
		}

		if len(sh.contentBlocks) == 0 && sh.responseText.Len() > 0 {
			sh.contentBlocks = append(sh.contentBlocks, map[string]interface{}{
				"type": "text",
				"text": sh.responseText.String(),
			})
		}
		if sh.contentBlocks == nil {
			sh.contentBlocks = make([]map[string]interface{}, 0)
		}

		var response interface{}
		if responseFormat == adapter.FormatOpenAI {
			response = buildOpenAINonStreamResponse(sh, req.Model, stopReason)
		} else {
			anthropicResponse := map[string]interface{}{
				"id":            sh.msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       sh.contentBlocks,
				"model":         req.Model,
				"stop_reason":   stopReason,
				"stop_sequence": nil,
				"usage": map[string]int{
					"input_tokens":  sh.inputTokens,
					"output_tokens": sh.outputTokens,
				},
			}
			response = anthropicResponse
		}

		if err := json.NewEncoder(w).Encode(response); err != nil {
			sh.markWriteError("nonstream_response", err)
			slog.Error("Failed to write JSON response", "error", err)
		}

	}

	// Sync state and update stats using helpers. A failed request with no
	// provider-reported usage must not turn the local input estimate into spend;
	// still count the request itself for operational history.
	statsInput, statsOutput := sh.inputTokens, sh.outputTokens
	if sh.requestFailed && !sh.useUpstreamUsage {
		statsInput, statsOutput = 0, 0
	}
	h.updateAccountStats(r.Context(), currentAccount, statsInput, statsOutput)

	// Audit log
	if h.auditLogger != nil {
		accountID := int64(0)
		channel := forcedChannel
		if currentAccount != nil {
			accountID = currentAccount.ID
			if channel == "" {
				channel = currentAccount.AccountType
			}
		}
		status := "success"
		if sh.requestFailed || (sh.finalStopReason == "" && !sh.hasReturn) {
			status = "error"
		}
		usageSource := audit.UsageSourceEstimated
		if sh.useUpstreamUsage {
			usageSource = audit.UsageSourceUpstream
		}
		metadata := map[string]interface{}{
			"stream": isStream,
		}
		for key, value := range sh.usageMetadata {
			metadata[key] = value
		}
		event := audit.Event{
			// One journal schema for every channel: the log centre must be able to
			// compare two channels' requests on the same fields.
			Kind:              audit.KindRequest,
			RequestID:         middleware.GetRequestID(r.Context()),
			Action:            "chat_request",
			APIKeyID:          middleware.APIKeyID(r.Context()),
			AccountID:         accountID,
			Model:             req.Model,
			Channel:           channel,
			ClientIP:          r.RemoteAddr,
			UserAgent:         r.UserAgent(),
			Duration:          time.Since(startTime).Milliseconds(),
			Status:            status,
			Metadata:          metadata,
			InputTokens:       sh.inputTokens,
			CachedInputTokens: sh.cachedInputTokens,
			CacheWriteTokens:  sh.cacheWriteTokens,
			ReasoningTokens:   sh.reasoningTokens,
			OutputTokens:      sh.outputTokens,
			TotalTokens:       sh.inputTokens + sh.outputTokens,
			UsageSource:       usageSource,
		}
		// Settle the reservation taken before the request and price the same
		// event, so the journal answers "what did this cost" and the key's
		// balance moves exactly once. An estimated row is never charged.
		if result, priced := middleware.SettleAPIKeyBilling(
			r.Context(), nil, req.Model, usageSource, int64(sh.inputTokens), int64(sh.cachedInputTokens), int64(sh.outputTokens),
		); priced {
			event.CostInUSDTicks = result.CostInUSDTicks
			event.PricingModel = result.Model
			event.PricingVersion = pricing.Version
		}
		h.auditLogger.Log(r.Context(), event)
	}
}

func toolChoiceDisablesTools(choice interface{}) bool {
	switch typed := choice.(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "none")
	case map[string]interface{}:
		return strings.EqualFold(strings.TrimSpace(fmt.Sprint(typed["type"])), "none")
	default:
		return false
	}
}

func randomSessionID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// Fallback to time-based if crypto/rand fails (unlikely)
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
