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
	"orchids-api/internal/provider"
	rtdebug "runtime/debug"
	"strings"
	"sync"
	"time"

	"encoding/json"

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
	auditLogger   audit.Logger

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
	OutputConfig   map[string]interface{}   `json:"output_config,omitempty"`
	Thinking       map[string]interface{}   `json:"thinking,omitempty"`
	ResponseFormat map[string]interface{}   `json:"response_format,omitempty"`
	ResponseText   map[string]interface{}   `json:"text,omitempty"`
	Include        []string                 `json:"include,omitempty"`
	PromptCacheKey string                   `json:"prompt_cache_key,omitempty"`
	ResponsesTools []map[string]interface{} `json:"x_responses_tools,omitempty"`
	MCPServers     []map[string]interface{} `json:"mcp_servers,omitempty"`
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

func NewWithLoadBalancer(cfg *config.Config, lb *loadbalancer.LoadBalancer) *Handler {
	h := &Handler{
		config:       cfg,
		loadBalancer: lb,
		connTracker:  loadbalancer.NewMemoryConnTracker(),
		clientCache:  newAccountClientCache(),
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
	// Surface an over-long shared-refusal budget once per config load. The value
	// is legal and can be right for a deployment with no edge proxy, but with a
	// 100s edge in front it turns a would-be 429 into a 520 for the caller, and
	// that is indistinguishable from an upstream outage in the access logs.
	WarnIfSharedRefusalBudgetExceedsEdge(SharedRefusalWaitBudget(cfg.SharedRefusalWaitBudgetMs))
	if h.clientCache != nil {
		h.clientCache.SetConfig(cfg)
	}
}

func (h *Handler) configSnapshot() *config.Config {
	if h == nil {
		return nil
	}
	return util.ReadSnapshot(&h.configMu, &h.config)
}

// SetAuditLogger replaces the default nop audit logger.
func (h *Handler) SetAuditLogger(al audit.Logger) { h.auditLogger = al }

// SetClientFactory sets the factory used by selectAccount to create provider-specific clients.
func (h *Handler) SetClientFactory(f ClientFactory) { h.clientFactory = f }

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

// materializeBlockField copies a streamed text/thinking block's accumulated
// content into its wire field before a non-streaming response is encoded. A
// block that never streamed anything still gets the field, empty.
func materializeBlockField(block map[string]interface{}, field string, builders []*strings.Builder, idx int) {
	if builder := builderAt(builders, idx); builder != nil {
		block[field] = builder.String()
		return
	}
	if _, ok := block[field]; !ok {
		block[field] = ""
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

	// Initialize debug logging
	cfg := h.configSnapshot()
	logger := debug.NewForContext(r.Context(), cfg.DebugEnabled, cfg.DebugLogSSE)
	defer logger.Close()
	verboseDiagnostics := logutil.VerboseDiagnosticsEnabled()

	// 1. Log the incoming Claude request
	logger.LogIncomingRequest(req)

	reqHash := h.computeRequestHash(r, bodyBytes)
	traceID := shortRequestTrace(reqHash)
	logutil.DebugIf(verboseDiagnostics, "Request fingerprint", "trace_id", traceID, "hash", reqHash, "path", r.URL.Path, "content_length", len(bodyBytes))

	// ...
	if ok, command := isCommandPrefixRequest(req); ok {
		logutil.DebugIf(verboseDiagnostics, "Handling command prefix request", "command", command)
		prefix := detectCommandPrefix(command)
		logger.LogEarlyExit("command_prefix", map[string]interface{}{
			"command": command,
			"prefix":  prefix,
		})
		writeCommandPrefixResponse(w, req, responseFormat, prefix, startTime, logger)
		return
	}

	if isTopicClassifierRequest(req) {
		logutil.DebugIf(verboseDiagnostics, "Handling topic classifier request locally")
		logger.LogEarlyExit("topic_classifier", map[string]interface{}{"mode": "local"})
		writeTopicClassifierResponse(w, req, responseFormat, startTime, logger)
		return
	}

	if isTitleGenerationRequest(req) {
		title := generateTopicTitle(extractUserText(req.Messages))
		logutil.DebugIf(verboseDiagnostics, "Handling title generation request locally", "title", title)
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
	logutil.DebugIf(verboseDiagnostics, "Request dispatch initialized", "trace_id", traceID, "path", r.URL.Path, "conversation_id", conversationKey, "model", req.Model, "stream", req.Stream)

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
	// upstream session whenever it changed, answer the "what is my current
	// working directory" question locally without
	// calling upstream, and rebase foreign tool paths onto it. Every one of those
	// behaviours is gone: the request that reached upstream is now the request the
	// caller wrote.
	if isSuggestionMode(req.Messages) {
		suggestion := buildLocalSuggestion(req.Messages)
		logutil.DebugIf(verboseDiagnostics, "Handling suggestion mode request locally", "suggestion", suggestion)
		logger.LogEarlyExit("suggestion_mode", map[string]interface{}{
			"mode":       "local",
			"suggestion": suggestion,
		})
		writeSuggestionModeResponse(w, req, responseFormat, startTime, logger)
		return
	}

	// WorkBuddy, Qoder and Cline forward raw OpenAI-style messages: their
	// endpoint is OpenAI-shaped and the caller's request is passed upstream
	// verbatim. The path or the model names the channel before selection; the
	// selected account confirms it afterwards.
	preSelectChannel := passthroughChannelName(targetChannel)
	protocolControls := upstream.UpstreamRequest{
		ResponseFormat: req.ResponseFormat, ResponseText: req.ResponseText,
		Include: req.Include, PromptCacheKey: req.PromptCacheKey, ResponsesTools: req.ResponsesTools,
	}
	if format, ok := req.OutputConfig["format"].(map[string]interface{}); ok && len(protocolControls.ResponseFormat) == 0 && len(protocolControls.ResponseText) == 0 {
		protocolControls.ResponseText = map[string]interface{}{"format": format}
		req.ResponseText = protocolControls.ResponseText
	}
	if len(req.MCPServers) > 0 {
		apperrors.New("invalid_request_error", "MCP servers require a native Responses provider", http.StatusBadRequest).WriteResponse(w)
		return
	}
	if err := protocolControls.ValidateProtocolControls(preSelectChannel); err != nil {
		apperrors.New("invalid_request_error", err.Error(), http.StatusBadRequest).WriteResponse(w)
		return
	}
	// A strict structured-output request on a passthrough channel carries a
	// contract upstream only honors on some models. The schema is compiled
	// here, before account selection, so one compiled schema can both instruct
	// the model and check its answer; a non-strict request yields nil and every
	// path below is unchanged.
	structured := upstream.NewStructuredOutput(protocolControls)
	preSelectWorkBuddyRequest := preSelectChannel == "workbuddy"
	preSelectQoderRequest := preSelectChannel == "qoder"
	preSelectClineRequest := preSelectChannel == "cline"
	// Suggestion mode answered and returned above, so the gates below can only be
	// triggered by tool_choice or by a tool_result-only follow-up; thinking stays
	// suppressed only by configuration.
	noThinking := cfg.SuppressThinking
	gateNoTools := false
	toolGateReasons := make([]string, 0, 2)
	toolGateMessage := ""
	if toolChoiceDisablesTools(req.ToolChoice) {
		gateNoTools = true
		toolGateReasons = append(toolGateReasons, "tool_choice_none")
		toolGateMessage = buildToolGateMessage(req.Messages)
	}
	if lastUserIsToolResultFollowup(req.Messages) {
		if preSelectChannel != "" {
			logutil.DebugIf(verboseDiagnostics, "tool_gate: keeping tools for passthrough tool_result follow-up")
		} else {
			gateNoTools = true
			toolGateReasons = append(toolGateReasons, "tool_result_followup")
			toolGateMessage = buildToolGateMessage(req.Messages)
			logutil.DebugIf(verboseDiagnostics, "tool_gate: disabled tools for tool_result-only follow-up")
		}
	}
	effectiveTools := req.Tools
	if gateNoTools {
		effectiveTools = nil
		logutil.DebugIf(verboseDiagnostics, "tool_gate: disabled tools", "reasons", toolGateReasons)
	}
	// Select account (initial selection)
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
	logutil.DebugIf(verboseDiagnostics, "Checkpoint: selectAccount success")

	// The selected account confirms the family the pre-selection guess named: the
	// account type is authoritative when the path did not pin a channel.
	accountChannel := ""
	if currentAccount != nil {
		accountChannel = passthroughChannelName(currentAccount.AccountType)
	}
	// The label follows the priority this gate has always used: WorkBuddy, then
	// Qoder, then Cline. Passthrough channels do not trim history/tool results.
	passthroughChannel := ""
	switch {
	case preSelectWorkBuddyRequest || accountChannel == "workbuddy":
		passthroughChannel = "workbuddy"
	case preSelectQoderRequest || accountChannel == "qoder":
		passthroughChannel = "qoder"
	case preSelectClineRequest || accountChannel == "cline":
		passthroughChannel = "cline"
	}
	logutil.DebugIf(verboseDiagnostics && passthroughChannel != "", "Checkpoint: passthrough, skip context trimming", "channel", passthroughChannel)
	logutil.DebugIf(verboseDiagnostics, "Checkpoint: message processing done")

	// The account slot was atomically reserved with selection. Keeping the
	// reservation from this point through the complete SSE prevents concurrent
	// requests from racing past a per-account limit before either increments it.

	// Build the prompt (V2 Markdown format)
	startBuild := time.Now()
	logutil.DebugIf(verboseDiagnostics, "Starting prompt build...", "conversation_id", conversationKey)
	// Map the model (so the upstream request and the prompt agree)
	mappedModel := mapModel(req.Model)
	if passthroughChannel != "" {
		mappedModel = strings.TrimSpace(req.Model)
	}

	builtPrompt := strings.TrimSpace(extractUserText(req.Messages))
	if builtPrompt == "" {
		// A passthrough channel labels its placeholder; an empty one leaves the
		// bare "request".
		builtPrompt = strings.TrimSpace(passthroughChannel + " request")
	}
	buildDuration := time.Since(startBuild)
	if verboseDiagnostics {
		slog.Debug("Prompt build completed", "duration", buildDuration)
		slog.Debug("[Performance] BuildPromptAndHistory", "duration", buildDuration)
	}

	logutil.DebugIf(verboseDiagnostics, "Model mapping", "original", req.Model, "mapped", mappedModel)

	isStream := req.Stream

	if isStream {
		// Check the complete middleware chain before committing SSE headers. A
		// wrapper may expose Flush while its underlying writer cannot actually
		// flush, so unwrap to the real server writer before accepting the stream.
		if !responseWriterSupportsFlush(w) {
			apperrors.New("api_error", "Streaming not supported by underlying connection", http.StatusInternalServerError).WriteResponse(w)
			return
		}
		// Set the SSE response headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		streamingStarted = true
	} else {
		w.Header().Set("Content-Type", "application/json")
	}

	// State management
	// msgID is now managed by streamHandler

	upstreamMessages := append([]prompt.Message(nil), req.Messages...)

	if gateNoTools {
		builtPrompt = injectToolGate(builtPrompt, toolGateMessage)
	}

	// 2. Log the converted prompt
	logutil.DebugIf(verboseDiagnostics, "Checkpoint: LogConvertedPrompt")
	logger.LogConvertedPrompt(builtPrompt)

	breakdown := estimateInputTokenBreakdown(builtPrompt, effectiveTools)
	breakdownProfile := passthroughChannel
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

	// Token count (for the leading usage display)
	inputTokens := breakdown.Total

	upstreamCtx, cancelUpstream := util.WithAttemptTimeout(r.Context(), time.Duration(cfg.RequestTimeout)*time.Second)
	defer cancelUpstream()
	r = r.WithContext(upstreamCtx)
	sh := newStreamHandler(cfg, w, logger, noThinking, isStream, responseFormat)
	sh.cancelUpstream = cancelUpstream
	sh.setAllowedToolNames(declaredToolNames(effectiveTools))
	if preSelectQoderRequest {
		sh.setSurfaceToolRejects(true)
	}
	// effectiveTools is either req.Tools or nil, so a request with no tools of its
	// own has nothing to report here.
	if len(req.Tools) > 0 {
		sh.setClientTools(req.Tools)
	}
	sh.setDisallowToolCalls(gateNoTools)
	sh.setEmptyOutputFallback(successfulFileMutationToolResultFallback(upstreamMessages))
	// The compiled schema gates the finished answer; a request that declared no
	// schema installs a nil check and behaves exactly as before.
	sh.setStructuredOutput(structured.Validate)
	sh.setUsageTokens(inputTokens, -1) // Correctly initialize input tokens
	defer sh.release()

	// The opening frame is deliberately NOT written here. Writing it before the
	// upstream is called committed 200 and a role chunk to every streaming client,
	// so a shared queue refusal arriving afterwards could only be reported in band
	// -- which clients surface as a truncated stream instead of a retryable 429.
	// Keep-alive ticks also wait until actual content has opened the stream, so
	// a queue refusal after 15 seconds can still carry a real HTTP status.
	sh.pendingModel = req.Model

	logutil.DebugIf(verboseDiagnostics, "New request received")

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
		// A per-request chat session id, reused across account switches so the
		// upstream keeps one conversation for this downstream request.
		chatSessionID := "chat_" + randomSessionID()
		maxRetries := cfg.MaxRetries
		if maxRetries < 0 {
			maxRetries = 0
		}
		retryDelay := time.Duration(cfg.RetryDelay) * time.Millisecond
		retriesRemaining := maxRetries
		budgetCtx, attemptBudget := upstream.WithAttemptBudget(r.Context(), maxRetries+1)
		r = r.WithContext(budgetCtx)
		// sharedRefusalWaited accumulates only the waits spent on a refusal that
		// every account shares, which is bounded separately from maxRetries.
		var sharedRefusalWaited time.Duration

		// Publish the model this request resolved to, so the per-minute
		// aggregation can attribute the outcome to a model rather than only to a
		// channel. The middleware cannot read the body itself.
		r = r.WithContext(middleware.WithRequestModel(r.Context(), mappedModel))

		payloadMessages := upstreamMessages
		// The schema instruction is prepended rather than appended: a channel
		// that truncates a long system array keeps the shape the answer must
		// have, not the caller's prose behind it.
		payloadSystem := upstream.PrependSystemHint(req.System, structured.SystemHint())

		upstreamReq := upstream.UpstreamRequest{
			ResponseFormat:    req.ResponseFormat,
			ResponseText:      req.ResponseText,
			Include:           req.Include,
			PromptCacheKey:    req.PromptCacheKey,
			ResponsesTools:    req.ResponsesTools,
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
			accountType, accountName := "", ""
			if currentAccount != nil {
				accountID, accountType, accountName = currentAccount.ID, currentAccount.AccountType, currentAccount.Name
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
				slog.Debug("Using SendRequestWithPayload")
			}

			callsBefore := attemptBudget.Used()
			callCtx := upstream.WithAttemptObserver(r.Context(), func(failed bool) {
				middleware.RecordUpstreamAttempt(r.Context(), accountID, failed)
			})
			err = apiClient.SendRequestWithPayload(callCtx, upstreamReq, primaryHandler, logger)
			// The same account id the diagnostics above reported, with or without
			// diagnostics enabled.
			if attemptBudget.Used() == callsBefore {
				middleware.RecordUpstreamAttempt(r.Context(), accountID, err != nil)
			}
			logutil.DebugIf(verboseDiagnostics, "Upstream client returned", "trace_id", traceID, "attempt", upstreamReq.Attempt, "error", err)

			if err == nil {
				sh.forceFinishIfMissing()
				logutil.DebugIf(verboseDiagnostics, "Upstream attempt completed", "trace_id", traceID, "attempt", upstreamReq.Attempt)
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
					errClass.Category, apperrors.PublicMessage(errStr), upstreamRetryAfter(err))
				return
			}

			// Check for non-retriable errors
			slog.Error("Request error", "trace_id", traceID, "attempt", upstreamReq.Attempt, "error", err, "category", errClass.Category, "retryable", errClass.Retryable)
			// One decision for both questions this error raises: whether the
			// account keeps its place in the pool, and whether the request may be
			// retried. The scheduler reads the same policy, so a failure cannot be
			// "cooling down" for one entrance and "retryable" for the other.
			verdict := accountpolicy.Classify(currentAccount, err, req.Model)
			// Mark the account status (auth-class errors are always marked,
			// whether or not they are retryable)
			if currentAccount != nil && h.loadBalancer != nil && h.loadBalancer.Store != nil {
				if verdict.Scope == accountpolicy.ScopeModel && verdict.Model != "" && verdict.Cooldown > 0 {
					// WorkBuddy code 6004 is a model-frequency limit. Persist only
					// that model's cooldown; applying an empty account status here
					// would either be skipped or accidentally clear unrelated state.
					// The verdict's kind travels with the deadline: the selection
					// layer cannot see this request, and has to know whether the
					// model is throttled or simply not covered by the plan.
					store.RecordModelCooldownWithReason(currentAccount, verdict.Model, time.Now().Add(verdict.Cooldown), verdict.ModelCooldownKind)
					if persistErr := h.loadBalancer.Store.UpdateAccount(r.Context(), currentAccount); persistErr != nil {
						slog.Warn("persist model cooldown failed", "account_id", currentAccount.ID, "model", verdict.Model, "error", persistErr)
					}
				} else if verdict.Status != "" {
					logutil.DebugIf(verboseDiagnostics, "标记账号状态", "account_id", currentAccount.ID, "status", verdict.Status, "scope", string(verdict.Scope), "category", errClass.Category)
					// Apply keeps the status and its operator-facing reason
					// together, so the account table can explain the cooldown.
					verdict.Apply(currentAccount)
					// WorkBuddy keeps a spent account in the pool for its free tier
					// only. When the upstream refuses that tier too — production
					// answers 14018 "Credits exhausted" for a zero-balance plan on a
					// confirmed free model — the tier is not available on this account
					// either, and every later request pays for the same upstream
					// rejection before switching. Scope the verdict to the model, so
					// the pool stops offering this account for it while the balance is
					// spent; the account keeps its free-only capability state.
					if verdict.Status == store.AccountStatusWorkBuddyQuotaExhausted &&
						provider.IsFreeModel("workbuddy", currentAccount, upstreamReq.Model) {
						// Hold the model out of this account until its plan resets, but
						// never past the cap below: the reset comes from the upstream's
						// wall clock, whose zone the gateway cannot verify, so a misread
						// boundary must not park a model for hours longer than the
						// refusal deserves.
						const freeTierMaxHold = 6 * time.Hour
						freeTierHold := freeTierMaxHold
						if reset := currentAccount.WorkBuddyQuota.ResetAt; reset.After(time.Now()) {
							if until := time.Until(reset); until < freeTierHold {
								freeTierHold = until
							}
						}
						slog.Warn("WorkBuddy free tier refused on a spent account; scoping the model out",
							"account_id", currentAccount.ID, "model", upstreamReq.Model, "hold", freeTierHold)
						store.RecordModelCooldownWithReason(currentAccount, upstreamReq.Model, time.Now().Add(freeTierHold), store.ModelCooldownUnavailable)
					}
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
					sh.reportRequestFailure("Reporting unexpected upstream cancellation", "server", "Upstream request was canceled unexpectedly", 0)
					return
				}
				sh.reportRequestFailure("Reporting non-retriable upstream failure",
					errClass.Category, apperrors.PublicMessage(errStr), upstreamRetryAfter(err))
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
			if retriesRemaining <= 0 || attemptBudget.Remaining() <= 0 {
				if currentAccount != nil && h.loadBalancer != nil {
					slog.Error("Account request failed, max retries reached", "account", currentAccount.Name)
				}
				// Same rule as the non-retriable branch above: with nothing sent yet
				// this is a gateway failure, and the client sees it as one.
				sh.reportRequestFailure("Reporting that retries are exhausted",
					errClass.Category, apperrors.PublicMessage(errStr), upstreamRetryAfter(err))
				return
			}
			retriesRemaining--
			slog.Warn(
				"Retrying upstream request without prior output",
				"trace_id", traceID,
				"attempt", upstreamReq.Attempt,
				"category", errClass.Category,
				"switch_account", verdict.SwitchAccount,
				"retries_remaining", retriesRemaining,
			)
			if verdict.SwitchAccount && currentAccount != nil && h.loadBalancer != nil {
				prevClient := apiClient
				prevAccount := currentAccount
				if _, ok := failedAccountSet[currentAccount.ID]; !ok {
					failedAccountSet[currentAccount.ID] = struct{}{}
					failedAccountIDs = append(failedAccountIDs, currentAccount.ID)
				}
				slog.Warn("Account request failed, switching account", "account", currentAccount.Name, "unsuccessful_attempts", len(failedAccountIDs))

				// Release the old account's connection count
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
			sharedRefusal := isSharedUpstreamRefusalClass(errClass)
			// sharedRefusalWait is the selected interval, charged against the
			// budget. The jitter below is added only to the sleep: it exists to
			// decorrelate wake-ups, and charging it made the last reachable
			// window unreachable (a 30s hint plus up to 5s of jitter spent 35s
			// against a 60s budget that had already paid 30s).
			sharedRefusalWait := time.Duration(0)
			qoderIntervalMs := 0
			if cfg != nil && errClass.Category == "upstream_queue" {
				qoderIntervalMs = cfg.QoderQueueRetryIntervalMs
			}
			if retryDelayForAttempt > 0 && sharedRefusal {
				// A configured Qoder interval overrides its provider hint. Other
				// channels keep their existing early-probe policy.
				sharedRefusalWait = sharedRefusalWaitForChannel(retryDelayForAttempt, attempt+1, targetChannel, qoderIntervalMs)
			}
			// configSnapshot reports nil for a nil handler, so the knob is read
			// defensively: losing the setting must fall back to the built-in
			// bound, not panic inside the retry loop.
			budgetMs := 0
			if cfg != nil {
				budgetMs = cfg.SharedRefusalWaitBudgetMs
			}
			waitBudget := SharedRefusalWaitBudget(budgetMs)
			// A shared refusal is a gate on the upstream's side, not this account's
			// throttle, so the wait is spent on the same account and can repeat.
			// Bound the total by the shortest deadline in front of this process,
			// which is the edge proxy's origin timeout, not the caller's patience:
			// answering past the edge's limit does not give the caller the work,
			// it gives it a 520 from the edge.
			if sharedRefusal && sharedRefusalWait > 0 && !sharedRefusalWaitAllowedWithin(sharedRefusalWaited, sharedRefusalWait, waitBudget) {
				slog.Warn("Shared upstream refusal exceeded the wait budget; answering now",
					"trace_id", traceID,
					"waited", sharedRefusalWaited,
					"next", sharedRefusalWait,
					"budget", waitBudget,
					"category", errClass.Category,
				)
				sh.reportRequestFailure("Reporting a shared refusal after the wait budget",
					errClass.Category, apperrors.PublicMessage(errStr), upstreamRetryAfter(err))
				return
			}
			if sharedRefusalWait > 0 {
				retryDelayForAttempt = sharedRefusalSleepForChannel(sharedRefusalWait, targetChannel, qoderIntervalMs)
			}
			// Holding this account's concurrency slot through the wait starves the
			// pool: the slot is reserved for the whole request, so ten requests
			// waiting out a gate would occupy one account completely while the rest
			// of the pool idled. Release it for the wait and take it back before the
			// next attempt; if it is gone by then the account really is busy.
			slotReleasedForWait := false
			if retryDelayForAttempt > 0 && sharedRefusal && trackedAccountID != 0 {
				h.releaseTrackedAccount(trackedAccountID)
				trackedAccountID = 0
				slotReleasedForWait = true
			}
			waitStarted := time.Now()
			waitCompleted := true
			if retryDelayForAttempt > 0 {
				waitCompleted = util.SleepWithContext(r.Context(), retryDelayForAttempt)
				debug.RecordWait(r.Context(), errClass.Category, retryDelayForAttempt, time.Since(waitStarted), !waitCompleted)
			}
			if !waitCompleted {
				sh.finishResponse("end_turn")
				return
			}
			if sharedRefusal {
				sharedRefusalWaited += sharedRefusalWait
				if sharedRefusalWait == 0 {
					// No hint to charge, but the attempt is still repeated: count
					// the delay actually spent so a hintless gate cannot loop
					// forever without moving the budget.
					sharedRefusalWaited += retryDelayForAttempt
				}
			}
			if slotReleasedForWait && currentAccount != nil {
				reacquiredID, acquired := h.tryAcquireTrackedAccount(currentAccount)
				if !acquired {
					// Another request took the slot while this one waited. Ending
					// here is the honest answer: the account is at its limit, and
					// retrying would either exceed that limit or make the caller
					// wait through a second gate.
					slog.Warn("Account became busy during a shared-refusal wait; ending the request",
						"trace_id", traceID, "account_id", currentAccount.ID, "account", currentAccount.Name)
					sh.reportRequestFailure("Reporting a busy pool after a shared-refusal wait",
						"rate_limit", apperrors.PoolBusyMessage, 0)
					return
				}
				trackedAccountID = reacquiredID
			}
			attempt++
		}
	}

	run()

	// Ensure a final response
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
				materializeBlockField(sh.contentBlocks[i], "text", sh.textBlockBuilders, i)
			case "thinking":
				materializeBlockField(sh.contentBlocks[i], "thinking", sh.thinkingBlockBuilders, i)
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
