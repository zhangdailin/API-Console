package handler

import (
	"context"
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

	"encoding/json"

	"orchids-api/internal/adapter"
	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/logutil"
	"orchids-api/internal/middleware"
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

func shortRequestTrace(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func (h *Handler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	forcedChannel := channelFromPath(r.URL.Path)
	if forcedChannel == "" {
		apperrors.New("not_found_error", "Provider route not found", http.StatusNotFound).WriteResponse(w)
		return
	}

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

	effort := requestReasoningEffort(req)
	req.Model = h.resolveEffortModelVariant(r.Context(), req.Model, effort, forcedChannel)
	_, err = h.validateModelAvailability(r.Context(), req.Model, forcedChannel)
	if err != nil {
		apperrors.New("invalid_request_error", err.Error(), http.StatusBadRequest).WriteResponse(w)
		return
	}
	targetChannel := strings.TrimSpace(forcedChannel)
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
	// verbatim. The path determines the channel before account selection.
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
	preSelectQoderRequest := preSelectChannel == "qoder"
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

	var e *messageExecution
	apiClient, currentAccount, releaseClient, trackedAccountID, err := h.acquireReservedAccountSelection(r.Context(), targetChannel, true, failedAccountIDs, accountSelectionOptions{
		ModelID: strings.TrimSpace(req.Model),
	})
	// The client is held for the whole request: a credential change during it
	// retires the client and closes it here, after the request finished.
	defer func() {
		if e != nil {
			e.releaseClient()
		} else {
			releaseClient()
		}
	}()
	defer func() {
		if e != nil {
			h.releaseTrackedAccount(e.trackedAccountID)
		} else {
			h.releaseTrackedAccount(trackedAccountID)
		}
	}()
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

	passthroughChannel := preSelectChannel
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
	sh.setDisallowToolCalls(gateNoTools)
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

	e = &messageExecution{h: h, r: r, cfg: cfg, sh: sh, req: req, mappedModel: mappedModel, targetChannel: targetChannel, forcedChannel: forcedChannel, traceID: traceID, conversationKey: conversationKey, builtPrompt: builtPrompt, effort: effort, verboseDiagnostics: verboseDiagnostics, gateNoTools: gateNoTools, upstreamMessages: upstreamMessages, effectiveTools: effectiveTools, structured: structured, logger: logger, apiClient: apiClient, currentAccount: currentAccount, releaseClient: releaseClient, trackedAccountID: trackedAccountID, failedAccountIDs: failedAccountIDs, failedAccountSet: failedAccountSet}
	e.run()
	r, currentAccount, releaseClient, trackedAccountID = e.r, e.currentAccount, e.releaseClient, e.trackedAccountID

	h.finishMessages(w, r, sh, req, currentAccount, forcedChannel, isStream, responseFormat, startTime, logger)
}
