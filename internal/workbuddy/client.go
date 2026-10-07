// Package workbuddy implements the WorkBuddy international backend
// (www.workbuddy.ai, isOversea=true) as an upstream provider.
//
// The wire protocol is OpenAI-shaped but not OpenAI-compatible in three ways
// that the client must honour:
//
//  1. messages[0] must be a system message; this client always emits one. Code
//     11128 is the upstream's policy gate, not a message-order error: it answers
//     "Illegal API invocation from an unapproved channel" with displayMsg "The
//     request was blocked by security policy", and it also fires when the
//     prompt carries a first-party Anthropic client marker (see messages.go).
//  2. stream must be true; the endpoint always answers with SSE.
//  3. tool_choice is a plain string (an object form is rejected with
//     code=11101), and the "developer" role is not in the accepted role
//     whitelist.
//
// Errors travel inside the {code,msg,requestId,data} envelope; code != 0 is a
// business error even when HTTP status is 200. 6004 is a per-model frequency
// limit (the account stays usable on other models) and 12153 means the stored
// session is dead.
package workbuddy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"orchids-api/internal/httpclient"

	"strings"
	"sync"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// DefaultBaseURL is the international deployment host.
const DefaultBaseURL = "https://www.workbuddy.ai"

// DefaultReasoningEffort stands in when the client did not state an effort.
//
// The upstream only routes its chain of thought into the reasoning channel when
// reasoning_effort is present. With the field omitted it writes the scratchpad
// into delta.content instead, and no reader on this channel can tell that prose
// apart from the answer, so the model's private reasoning reaches the client as
// visible text. Cline answers the same missing-field shape with the same value
// (cline.DefaultReasoningEffort), so the two dialects stay aligned on the
// default; only an explicit "none" differs, which buildBody keeps omitted.
const DefaultReasoningEffort = "high"

const (
	defaultModel   = "default-model"
	clientVersion  = "5.5.4"
	cliVersion     = "2.137.1"
	clientUA       = "WorkBuddy/" + clientVersion + " WorkBuddy AI/" + clientVersion + " CLI/" + cliVersion
	originReferer  = "https://www.workbuddy.ai"
	defaultSystem  = "You are a helpful assistant."
	minRefreshLead = time.Minute
	streamIdle     = 5 * time.Minute
)

// Business error codes observed from the international backend.
const (
	CodeLoginPending  = 11217
	CodeModelThrottle = 6004
	// CodePolicyBlocked is the upstream's content/security gate. It answers
	// "Illegal API invocation from an unapproved channel" and is what a request
	// carrying a first-party Anthropic client marker gets back.
	CodePolicyBlocked = 11128
	CodeSessionDead   = 12153
)

// Client is one WorkBuddy account's upstream client.
type Client struct {
	httpClient     *http.Client
	baseURL        string
	requestTimeout time.Duration
	streamIdle     time.Duration
	account        *store.Account
	accountStore   AccountUpdater

	creds   Credentials
	updater *tokenUpdater
	mu      sync.Mutex
}

// NewFromAccount builds a client for the given account. cfg supplies proxy and
// timeout settings only; the account supplies the credentials.
func NewFromAccount(acc *store.Account, cfg *config.Config) *Client {
	timeout := 5 * time.Minute
	if cfg != nil && cfg.RequestTimeout > 0 {
		timeout = time.Duration(cfg.RequestTimeout) * time.Second
		if timeout < 30*time.Second {
			timeout = 30 * time.Second
		}
	}

	proxyFunc := http.ProxyFromEnvironment
	proxyKey := "direct"
	baseURL := DefaultBaseURL
	if cfg != nil {
		proxyFunc = httpclient.ProxyFuncFromConfig(cfg)
		proxyKey = httpclient.GenerateProxyKeyFromConfig(cfg)
		if override := strings.TrimSpace(cfg.WorkBuddyBaseURL); override != "" {
			baseURL = strings.TrimRight(override, "/")
		}
	}

	var accountSnapshot *store.Account
	if acc != nil {
		copied := *acc
		accountSnapshot = &copied
	}
	return &Client{
		httpClient:     httpclient.GetSharedHTTPClientWithLimits(proxyKey+"|workbuddy-chat", timeout, proxyFunc, cfg != nil && cfg.WorkBuddyHTTP2Enabled, cfg),
		baseURL:        baseURL,
		requestTimeout: timeout,
		streamIdle:     streamIdle,
		account:        accountSnapshot,
		creds:          ResolveCredentials(acc),
	}
}

// SetAccountStore lets the client persist a rotated refresh token (Keycloak
// rotates it on every refresh, so dropping the new value breaks the account).
func (c *Client) SetAccountStore(s AccountUpdater) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.accountStore = s
	updater := c.updater
	c.mu.Unlock()
	if updater != nil {
		updater.SetAccountStore(s)
	}
}

// Close satisfies the shared upstream client lifecycle. The HTTP transport is
// process-wide, so a client owns no resources to close.
func (c *Client) Close() {}

// monitorStreamIdle is the WorkBuddy spelling of the shared idle monitor. The
// label is the only per-channel difference: it is what an operator reads in the
// timeout error.
func monitorStreamIdle(body io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	return httpclient.MonitorReadIdle(body, idle, cancel, "workbuddy")
}

// SendRequestWithPayload streams one chat completion to the caller.
func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	return c.runChat(ctx, req, c.requestTimeout, onMessage, logger)
}

func (c *Client) runChat(ctx context.Context, req upstream.UpstreamRequest, timeout time.Duration, onMessage func(upstream.SSEMessage), logger *debug.Logger) (sendErr error) {
	if c == nil {
		return fmt.Errorf("workbuddy client is nil")
	}

	accessToken, err := c.ensureAccessToken(ctx)
	if err != nil {
		return err
	}

	body, err := c.buildBody(req)
	if err != nil {
		return err
	}

	url := c.baseURL + "/v2/chat/completions"
	if logger != nil && !logger.Capturing() {
		logger.LogUpstreamRequest(url, map[string]string{"provider": "workbuddy"}, body)
	}

	reqCtx, cancel := util.WithAttemptTimeout(ctx, timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create workbuddy request: %w", err)
	}
	applyChatHeaders(httpReq, accessToken, c.creds.UID, req.ConversationID, req.RequestID, req.TraceID)

	attempt := debug.BeginUpstream(ctx, httpReq.Method, httpReq.URL.String(), httpReq.Header, body)
	httpReq = attempt.TraceRequest(httpReq)
	finishAttempt, err := upstream.BeginAttempt(ctx)
	if err != nil {
		return err
	}
	defer func() { finishAttempt(sendErr) }()
	resp, err := c.httpClient.Do(httpReq)
	attempt.Response(resp, err)
	if resp != nil {
		resp.Body = attempt.CaptureBody(resp.Body)
	}
	if err != nil {
		return fmt.Errorf("failed to send workbuddy request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return apiErrorWithRetry(resp.StatusCode, raw, parseRetryAfter(resp.Header.Get("Retry-After")))
	}
	resp.Body = monitorStreamIdle(resp.Body, c.streamIdle, cancel)

	result, err := consumeStream(resp.Body, onMessage)
	if err != nil {
		if typed, ok := err.(*APIError); ok && typed.RetryDelay == 0 {
			typed.RetryDelay = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		return err
	}
	if !result.SawMeaningfulEvent {
		return fmt.Errorf("workbuddy API returned no usable stream events")
	}
	if onMessage != nil {
		event := map[string]interface{}{"finishReason": result.FinishReason()}
		if len(result.Usage) > 0 {
			event["usage"] = result.Usage
		}
		onMessage(upstream.SSEMessage{Type: "model.finish", Event: event})
	}
	return nil
}

func parseRetryAfter(value string) time.Duration {
	return httpclient.ParseRetryAfter(value, time.Now(), 30*time.Second)
}

// ensureAccessToken returns a usable bearer token, refreshing when the stored
// access token is missing or about to expire.
func (c *Client) ensureAccessToken(ctx context.Context) (string, error) {
	return c.tokenUpdater().Token(ctx, c.creds)
}

func (c *Client) tokenUpdater() *tokenUpdater {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.updater == nil {
		c.updater = newTokenUpdater(c.baseURL, c.httpClient, c.accountStore, c.account)
	}
	return c.updater
}

// buildBody renders the OpenAI-shaped request body the upstream expects.
func (c *Client) buildBody(req upstream.UpstreamRequest) ([]byte, error) {
	if err := req.ValidateProtocolControls("workbuddy"); err != nil {
		return nil, err
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = defaultModel
	}
	body := map[string]interface{}{
		"model":          model,
		"stream":         true,
		"stream_options": map[string]interface{}{"include_usage": true},
		"messages":       buildMessages(req),
	}
	if format := req.ChatResponseFormat(); len(format) > 0 {
		body["response_format"] = format
	}
	// Compatibility with the observed working WorkBuddy request shape:
	// leave cache selection and parallel tool scheduling to the upstream.
	if req.MaxTokens != nil {
		body["max_tokens"] = *req.MaxTokens
	} else {
		body["max_tokens"] = 8192
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.Stop != nil {
		body["stop"] = req.Stop
	}
	// The upstream accepts the OpenAI-style reasoning_effort hint. An omitted
	// field makes it inline the chain of thought into content, which surfaces as
	// answer text, so a default stands in when the client stated nothing; a
	// stated effort wins. "none" is a stated choice, not an absence: it means
	// the client explicitly asked for no reasoning, so the field stays out
	// rather than carrying a level the upstream would reject.
	effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if effort == "" {
		effort = DefaultReasoningEffort
	}
	if effort != "none" {
		body["reasoning_effort"] = effort
	}
	if conversationID := strings.TrimSpace(req.ConversationID); conversationID != "" {
		body["conversationId"] = conversationID
	}
	if req.Tools != nil && !req.NoTools {
		if tools := normalizeToolDefinitions(req.Tools); len(tools) > 0 {
			body["tools"] = tools
			body["tool_choice"] = normalizeToolChoice(req.ToolChoice)
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal workbuddy request: %w", err)
	}
	return raw, nil
}

// RefreshCredentials forces one token refresh and returns the rotated pair. It
// is used at login time to capture the durable refresh token when the
// authorization response did not include one.
func (c *Client) RefreshCredentials(ctx context.Context) (Credentials, error) {
	if c == nil {
		return Credentials{}, fmt.Errorf("workbuddy client is nil")
	}
	return c.tokenUpdater().RefreshNow(ctx, c.creds)
}
