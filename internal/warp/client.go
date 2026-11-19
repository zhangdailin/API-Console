// Package warp implements the official Warp device-login channel.
package warp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/httpclient"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

const (
	DefaultBaseURL = "https://app.warp.dev"
	clientVersion  = "v0.2026.08.19.08.15.stable_01"
)

var (
	errTransport   = errors.New("warp upstream transport failed")
	errResponse    = errors.New("warp upstream response rejected")
	errCredential  = errors.New("warp credential unavailable; sign in again")
	errPersistence = errors.New("warp credential persistence failed")

	// Parser failures are classified by identity, never by upstream error text.
	errStreamFinish = errors.New("warp stream finish rejected")
)

type AccountUpdater interface {
	UpdateWarpCredentials(context.Context, int64, store.WarpCredentialPatch) error
}

type Client struct {
	http                                       *http.Client
	base, firebaseBase, tokenBase, firebaseKey string
	account                                    *store.Account
	owner                                      AccountUpdater
	generationTimeout                          time.Duration
	streamIdleTimeout                          time.Duration
	bindings                                   *bindingStore
	recovery                                   *recoveryStore
	mu                                         sync.Mutex
}

func NewFromAccount(acc *store.Account, cfg *config.Config) *Client {
	c := &Client{bindings: newBindingStore(), base: DefaultBaseURL, firebaseBase: "https://identitytoolkit.googleapis.com", tokenBase: "https://securetoken.googleapis.com"}
	if acc != nil {
		copy := *acc
		copy.WarpModelIDs = append([]string(nil), acc.WarpModelIDs...)
		c.account = &copy
	}
	if cfg != nil {
		if cfg.WarpBaseURL != "" {
			c.base = strings.TrimRight(cfg.WarpBaseURL, "/")
		}
		if cfg.WarpFirebaseBaseURL != "" {
			c.firebaseBase = strings.TrimRight(cfg.WarpFirebaseBaseURL, "/")
		}
		if cfg.WarpTokenBaseURL != "" {
			c.tokenBase = strings.TrimRight(cfg.WarpTokenBaseURL, "/")
		}
		c.firebaseKey = strings.TrimSpace(cfg.WarpFirebaseAPIKey)
	}
	if c.firebaseKey == "" {
		c.firebaseKey = strings.TrimSpace(os.Getenv("ORCHIDS_WARP_FIREBASE_API_KEY"))
	}
	c.generationTimeout = cfg.UpstreamRequestTimeout()
	c.streamIdleTimeout = config.SharedStreamIdleTimeout
	headerTimeout := min(c.generationTimeout, c.streamIdleTimeout)
	shared := httpclient.GetSharedHTTPClientWithTimeouts(httpclient.GenerateProxyKeyFromConfig(cfg), c.generationTimeout, headerTimeout, httpclient.ProxyFuncFromConfig(cfg), false, cfg)
	local := *shared
	local.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.http = &local
	return c
}

func (c *Client) SetAccountStore(s AccountUpdater) { c.mu.Lock(); defer c.mu.Unlock(); c.owner = s }

func (c *Client) request(ctx context.Context, endpoint, contentType string, body []byte, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errTransport
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Warp-Client-ID", "warp-app")
	req.Header.Set("X-Warp-OS-Category", "Linux")
	req.Header.Set("X-Warp-OS-Name", "Linux")
	req.Header.Set("User-Agent", "")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType == "application/x-protobuf" {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Accept-Encoding", "identity")
	}
	client := c.http
	if contentType != "application/x-protobuf" {
		// Authentication and device-login calls keep their shorter operation budget.
		local := *client
		if local.Timeout <= 0 || local.Timeout > 90*time.Second {
			local.Timeout = 90 * time.Second
		}
		client = &local
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if contentType == "application/x-protobuf" && isGenerationTimeout(err) {
			return nil, errGenerationTimeout
		}
		return nil, errTransport
	}
	return resp, nil
}

func (c *Client) json(ctx context.Context, endpoint, contentType string, body []byte, token string, target any) error {
	resp, err := c.request(ctx, endpoint, contentType, body, token)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(b) > 2<<20 {
		return errResponse
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var d struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &d)
		switch d.Error {
		case "authorization_pending":
			return ErrAuthPending
		case "slow_down":
			return errSlowDown
		}
		return errResponse
	}
	if json.Unmarshal(b, target) != nil {
		return errResponse
	}
	return nil
}

func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), _ *debug.Logger) (err error) {
	prepared, err := validateRequest(req)
	if err != nil {
		return err
	}
	c.mu.Lock()
	supported := capabilities{}.SupportsModel(c.account, req.Model)
	c.mu.Unlock()
	if !supported {
		return apperrors.NewLocalRequestError("warp_model_catalog")
	}
	prepared.ContextWindow = c.modelContextWindow(req.Model)
	c.mu.Lock()
	accountID := int64(0)
	if c.account != nil {
		accountID = c.account.ID
	}
	c.mu.Unlock()
	var continuation *callBinding
	var continuationIDs []string
	var recovery *recoveryHead
	var recoveryRaw []byte
	explicitResume := upstream.ExplicitResume(ctx)
	digest := ""
	if explicitResume || len(prepared.ToolResults) > 0 {
		digest, err = recoveryDigest(req, prepared)
		if err != nil {
			return explicitRecoveryError()
		}
	}
	if explicitResume && len(prepared.ToolResults) == 0 {
		return explicitRecoveryError()
	}
	// Tool results require the original owned task; never pay for an unbound
	// replay as a silently unrelated stateless request.
	if len(prepared.ToolResults) > 0 {
		ids := make([]string, 0, len(prepared.ToolResults))
		for _, result := range prepared.ToolResults {
			ids = append(ids, result.ID)
		}
		binding, ok := c.bindings.lookupScoped(ctx, accountID, req.Model, ids)
		if !ok || binding.ConversationID == "" || len(binding.RootTaskContext) == 0 {
			return apperrors.New("warp_continuation_unavailable", "Warp tool continuation expired or belongs to a different owner, account or model; no upstream call was made", 409)
		}
		prepared.ConversationID, prepared.Query = binding.ConversationID, prepared.CurrentText
		prepared.RootTaskContext, prepared.ToolRounds = binding.RootTaskContext, binding.ToolRounds
		continuation, continuationIDs = &binding, ids
		if explicitResume {
			head, raw, loadErr := c.loadRecovery(ctx, accountID, req.Model, ids, digest)
			if loadErr != nil || head.ConversationID != binding.ConversationID || !recoverySnapshotMatches(binding.RootTaskContext, head.RootTaskContext) {
				return explicitRecoveryError()
			}
			recovery, recoveryRaw = &head, raw
			prepared.Resume = true
			prepared.RootTaskContext, prepared.ToolRounds = head.RootTaskContext, head.PaidTurns
		}
	}
	body, err := buildRequest(req.Model, prepared)
	if err != nil {
		return apperrors.NewLocalRequestError("warp_request_shape")
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.generationTimeout)
	defer cancel()
	if err := c.authorizeGeneration(ctx, token, req.Model); err != nil {
		return err
	}
	if recovery != nil {
		head, raw, reserveErr := c.reserveRecovery(ctx, *recovery, recoveryRaw)
		if reserveErr != nil {
			return explicitRecoveryError()
		}
		recovery, recoveryRaw = &head, raw
	} else if continuation != nil {
		if err := c.bindings.claimToolTurn(ctx, accountID, req.Model, continuationIDs, *continuation); err != nil {
			return apperrors.New("warp_continuation_conflict", "Warp tool results were already consumed or their continuation is unavailable; no upstream call was made", 409)
		}
	}
	var result streamResult
	paidStarted := false
	defer func() {
		if continuation != nil && (paidStarted || recovery != nil) {
			if c.finishRecovery(ctx, accountID, req.Model, continuationIDs, digest, *continuation, recovery, recoveryRaw, result, err != nil, req.RequestID) {
				err = recoveryAvailableError()
			}
		}
	}()
	finishAttempt, err := upstream.BeginAttempt(ctx)
	if err != nil {
		return err
	}
	defer func() { finishAttempt(err) }()
	upstream.NoteUpstreamCall(ctx, c.base+"/ai/multi-agent", req.Model)
	diagnostic := generationDiagnostic{Stage: "transport", ContentType: "missing", MCPToolsDeclared: len(prepared.Tools)}
	defer func() {
		if diagnostic.TimeoutReason == "" {
			diagnostic.TimeoutReason = generationTimeoutReason(err)
		}
		if capture := debug.FromContext(ctx); capture != nil {
			// This closed schema contains no wire payloads or credentials.
			data, _ := json.Marshal(diagnostic)
			capture.Set("3_warp_diagnostic.json", string(data))
		}
		if err != nil {
			attrs := []any{"stage", diagnostic.Stage,
				"upstream_http_status", diagnostic.UpstreamHTTPStatus,
				"parser_category", diagnostic.ParserCategory}
			if diagnostic.ModelMismatch != nil {
				attrs = append(attrs, "model_mismatch", diagnostic.ModelMismatch)
			}
			slog.WarnContext(ctx, "Warp generation failed; no automatic retry", attrs...)
		} else if diagnostic.ModelMismatch != nil {
			slog.WarnContext(ctx, "Warp accepted upstream model fallback or substitution", "model_mismatch", diagnostic.ModelMismatch)
		}
	}()
	paidStarted = true
	resp, err := c.request(ctx, c.base+"/ai/multi-agent", "application/x-protobuf", body, token)
	if err != nil {
		diagnostic.TimeoutReason = generationTimeoutReason(err)
		if isGenerationTimeout(err) {
			diagnostic.Stage, diagnostic.ParserCategory = generationParserFailure(err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return generationStageError(diagnostic.Stage, 0, err)
	}
	defer resp.Body.Close()
	diagnostic.UpstreamHTTPStatus = generationHTTPStatus(resp.StatusCode)
	kind, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	diagnostic.ContentType = generationContentType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK {
		diagnostic.Stage = "http_status"
		return generationStageError(diagnostic.Stage, resp.StatusCode, nil)
	}
	if kind != "text/event-stream" {
		diagnostic.Stage = "content_type"
		return generationStageError(diagnostic.Stage, resp.StatusCode, nil)
	}
	monitored, progress := httpclient.MonitorProgressIdle(resp.Body, c.streamIdleTimeout, cancel, "warp")
	defer monitored.Close()
	result, err = readStreamWithProgress(monitored, req.Model, prepared.RootTaskContext, progress)
	if err == nil || result.RecoverySafe {
		declared := make(map[string]bool, len(prepared.Tools))
		for _, tool := range prepared.Tools {
			declared[tool.Name] = true
		}
		for _, call := range result.ToolCalls {
			if !declared[call.Name] {
				result.RecoverySafe = false
				err = errStreamTool
				break
			}
		}
	}
	diagnostic.ParserEvents, diagnostic.TextBytes = result.Events, result.TextBytes
	diagnostic.MCPToolCalls = len(result.ToolCalls)
	diagnostic.ServerBookkeeping = result.ServerBookkeeping
	diagnostic.RejectAction, diagnostic.RejectMessage, diagnostic.RejectTool = result.RejectAction, result.RejectMessage, result.RejectTool
	diagnostic.Rejection = result.Rejection
	diagnostic.Failure = result.Failure
	diagnostic.FinishReason = result.FinishReason
	diagnostic.ConversationContext = result.ConversationContext
	c.mu.Lock()
	diagnostic.ModelMismatch = modelMismatchDetails(req.Model, result.ModelMismatch, c.account.WarpModelIDs)
	c.mu.Unlock()
	diagnostic.ModelUsed = c.recordObservedModel(ctx, req.Model, result.ModelUsed)
	if result.Usage.Measured {
		diagnostic.Usage = result.Usage.event()
	}
	if err != nil {
		diagnostic.TimeoutReason = generationTimeoutReason(err)
		diagnostic.Stage, diagnostic.ParserCategory = generationParserFailure(err)
		if errors.Is(err, httpclient.ErrStreamIdle("warp")) {
			return generationStageError(diagnostic.Stage, resp.StatusCode, err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return generationStageError(diagnostic.Stage, resp.StatusCode, err)
	}
	if ctx.Err() != nil {
		diagnostic.Stage = "incomplete"
		return ctx.Err()
	}
	if len(result.ToolCalls) > 0 {
		diagnostic.Stage, diagnostic.ParserCategory = "stream_protocol", 1
		if result.ConversationID == "" || len(result.RootTaskContext) == 0 {
			diagnostic.Failure = &streamFailure{Site: "binding_save", Reason: "missing_context"}
			return generationStageError("stream_protocol", resp.StatusCode, errStreamStream)
		}
		for _, call := range result.ToolCalls {
			if !c.bindings.saveScoped(ctx, accountID, req.Model, call.ID, callBinding{
				ConversationID: result.ConversationID, RequestID: result.RequestID, RunID: result.RunID, AccountID: accountID,
				RootTaskContext: result.RootTaskContext, ToolRounds: prepared.ToolRounds + 1,
			}) {
				diagnostic.Failure = &streamFailure{Site: "binding_save", Reason: "store_or_identity_conflict", MeasuredBytes: len(result.RootTaskContext), LimitBytes: continuationStateLimit}
				return generationStageError("stream_protocol", resp.StatusCode, errStreamStream)
			}
		}
	}
	diagnostic.Stage, diagnostic.ParserCategory = "success", 0
	// Warp buffers a full verified response. Apply authoritative usage before
	// opening downstream content, so Messages/Responses do not expose estimates.
	upstream.ApplyStreamUsage(onMessage, result.Usage.streamUsageEvent(), nil, nil)
	if result.Text != "" {
		upstream.EmitTextDelta(onMessage, result.Text, nil)
	}
	if onMessage != nil {
		for _, call := range result.ToolCalls {
			onMessage(upstream.SSEMessage{Type: "model.tool-call", Event: map[string]any{
				"toolCallId": call.ID, "toolName": call.Name, "input": call.Args,
			}})
		}
	}
	finishReason := "stop"
	if len(result.ToolCalls) > 0 {
		finishReason = "tool_use"
	}
	if onMessage != nil {
		onMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]any{"finishReason": finishReason}})
	}
	return nil
}

// generationDiagnostic is intentionally a closed, content-free projection. The
// usage map comes only from observedUsage.event's fixed numeric field allowlist.
type generationDiagnostic struct {
	Stage               string                         `json:"stage"`
	UpstreamHTTPStatus  int                            `json:"upstream_http_status"`
	ContentType         string                         `json:"content_type"`
	ParserCategory      int                            `json:"parser_category"`
	ParserEvents        int                            `json:"parser_events"`
	TextBytes           int                            `json:"text_bytes"`
	ServerBookkeeping   int                            `json:"server_bookkeeping"`
	MCPToolsDeclared    int                            `json:"mcp_tools_declared"`
	MCPToolCalls        int                            `json:"mcp_tool_calls"`
	RejectAction        int                            `json:"reject_action,omitempty"`
	RejectMessage       int                            `json:"reject_message,omitempty"`
	RejectTool          int                            `json:"reject_tool,omitempty"`
	Rejection           *streamRejection               `json:"rejection,omitempty"`
	Failure             *streamFailure                 `json:"failure,omitempty"`
	ModelMismatch       *modelMismatchDiagnostic       `json:"model_mismatch,omitempty"`
	ModelUsed           *modelMismatchDiagnostic       `json:"model_used,omitempty"`
	Usage               map[string]any                 `json:"usage,omitempty"`
	FinishReason        string                         `json:"finish_reason,omitempty"`
	ConversationContext *conversationContextDiagnostic `json:"conversation_context,omitempty"`
	TimeoutReason       string                         `json:"timeout_reason,omitempty"`
}

// Category values are stable local diagnostics, not upstream enum values:
// 0 none, 1 protocol, 2 tool/subagent, 3 incomplete, 4 finish, 5 limit,
// 6 reserved (formerly model substitution), 7 invalid usage, 8 timeout.
// Unknown errors fail closed as protocol.
func generationParserFailure(err error) (string, int) {
	switch {
	case isGenerationTimeout(err):
		return "timeout", 8
	case errors.Is(err, context.Canceled):
		return "incomplete", 3
	case err == nil:
		return "stream_protocol", 0
	case errors.Is(err, errStreamStream):
		return "stream_protocol", 1
	case errors.Is(err, errStreamTool):
		return "tool_or_subagent", 2
	case errors.Is(err, errStreamIncomplete):
		return "incomplete", 3
	case errors.Is(err, errStreamFinish), errors.Is(err, errStreamUpstream):
		return "finish", 4
	case errors.Is(err, errStreamLimit):
		return "limit", 5
	case errors.Is(err, errUsage):
		return "usage_invalid", 7
	default:
		return "stream_protocol", 1
	}
}

func generationStageError(stage string, status int, parseErr error) error {
	// Do not trust even the supplied stage: only fixed identifiers may escape.
	switch stage {
	case "transport", "http_status", "content_type", "stream_protocol", "tool_or_subagent", "incomplete", "finish", "limit", "usage_invalid", "timeout":
	default:
		stage = "stream_protocol"
	}
	_, category := generationParserFailure(parseErr)
	return apperrors.New("warp_generation_failed", "Warp generation failed at "+stage+"; no automatic retry", http.StatusBadGateway).WithDetails(map[string]any{
		"stage": stage, "upstream_http_status": generationHTTPStatus(status), "parser_category": category,
	})
}

func generationHTTPStatus(status int) int {
	if status < 100 || status > 599 {
		return 0
	}
	return status
}

func generationContentType(raw string) string {
	if raw == "" {
		return "missing"
	}
	if len(raw) > 256 {
		return "other"
	}
	kind, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return "other"
	}
	switch kind {
	case "text/event-stream", "application/json", "text/html", "text/plain", "application/octet-stream":
		return kind
	default:
		return "other"
	}
}

// modelContextWindow reports the window the catalog observed for this model. An
// unobserved window is left unset so upstream keeps the model's own default.
func (c *Client) modelContextWindow(model string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.account == nil || c.account.WarpModelWindows == nil {
		return 0
	}
	return c.account.WarpModelWindows[model]
}

func (c *Client) keyedURL(ctx context.Context, base, path string) (string, error) {
	if err := c.ensureProjectConfig(ctx); err != nil {
		return "", err
	}
	return base + path + "?" + url.Values{"key": {c.firebaseKey}}.Encode(), nil
}
