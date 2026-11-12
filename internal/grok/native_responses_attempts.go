package grok

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/config"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/responses"
	"orchids-api/internal/upstream"
)

func nativeAuditRequest(model string, payload map[string]any) *chatwire.Request {
	req := &chatwire.Request{Model: model, StartedAt: time.Now(), SourceOperation: "responses", ResponsesTools: responses.InterfaceMaps(payload["tools"]), MCPServers: responses.InterfaceMaps(payload["mcp_servers"])}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if effort, ok := reasoning["effort"].(string); ok && strings.TrimSpace(effort) != "" {
		req.ReasoningEffort = &effort
	}
	return req
}

// serveNativeResponses shares one physical-call budget across transport
// retries and reasoning recovery. Every account starts from immutable input
// and rejected output never establishes response ownership or replay.
func (h *Handler) serveNativeResponses(w http.ResponseWriter, r *http.Request, sess *chatAccountSession, pinned bool, spec ModelSpec, payload map[string]any, path string, namespaces responses.ToolNamespaces) (responseID string, captured []byte, result chatOutcome) {
	ctx := withClientTurnIndex(r.Context(), r)
	ctx, _ = upstream.WithAttemptBudget(ctx, config.GenerationMaxAttempts)
	immutable, err := cloneBuildPayload(payload)
	if err != nil {
		writeGrokUpstreamError(w, err)
		result.Err = err
		return
	}
	remaining, physical := h.accountSwitchBudget(), 0
	used := []int64{}
	var req *chatwire.Request
	oneCall := func(stage string) (*http.Response, error) {
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		if remaining <= 0 {
			return nil, fmt.Errorf("native response attempt budget exhausted")
		}
		remaining--
		physical++
		used = append(used, sess.acc.ID)
		started := time.Now()
		response, failure := h.buildClient().doResponsesAt(grokbase.WithReasoningDiagnostics(ctx, payload), sess.acc, path, payload)
		if stage != "" {
			h.auditAttemptDiagnostic(ctx, sess.acc, ProviderBuild, physical, started, failure, stage, response, nil, "")
		}
		return response, failure
	}
	request := func() (*http.Response, error) {
		if err := prepareBuildPayload(immutable, payload, sess.acc, spec.UpstreamModel); err != nil {
			return nil, err
		}
		ctx = grokbase.WithReasoningDiagnostics(ctx, payload)
		req = nativeAuditRequest(spec.ID, payload)
		initialStarted := time.Now()
		response, failure := oneCall("")
		if failure == nil || !isReasoningReplayDecodeError(failure) || preservesClientCompaction(payload, failure) || remaining <= 0 {
			return response, failure
		}
		h.auditAttemptDiagnostic(ctx, sess.acc, ProviderBuild, physical, initialStarted, failure, "reasoning_decode_rejected", response, nil, "")
		if session := grokbase.SessionFromContext(ctx); session.Key != "" {
			h.clearReasoningReplay(ctx, spec.ID, session.Key)
		}
		lastStage := ""
		response, failure = recoverChatReasoning(payload, failure, func(stage string) (*http.Response, error) {
			lastStage = stage
			return oneCall(stage)
		})
		if failure == nil && response != nil {
			response.Header.Set("X-Grok2API-Reasoning-Recovery", lastStage)
		}
		return response, failure
	}
	openNext := func(_ []int64) (*chatAccountSession, error) {
		return h.openCLIAccountSession(ctx, used, spec.UpstreamModel)
	}
	// Hosted tools may already have executed unless the upstream explicitly
	// rejected the free allowance before generation. Continuations always
	// remain on their creating account, including after such a rejection.
	continuation := pinned || chatwire.ParseLooseStringAny(immutable["previous_response_id"]) != ""
	limit := remaining
	var allowSwitch func(error) bool
	if continuation {
		limit = 1
	} else if requestReplayUnsafe(nativeAuditRequest(spec.ID, immutable)) {
		allowSwitch = isFreeUsageExhaustedError
	}
	resp, failure := h.retryWithAccountSwitchWhen(ctx, sess, 1500*time.Millisecond, request, openNext, nil, limit, allowSwitch)
	if failure != nil {
		result.Err = failure
		writeGrokUpstreamFailure(w, upstreamHTTPResponseStatus(failure), failure)
		return
	}
	h.syncGrokQuota(sess.acc, resp.Header)
	var keepalive *httpserver.SSEKeepalive
	delivery := http.ResponseWriter(w)
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		keepalive = httpserver.NewSSEKeepalive(w, httpserver.SSEKeepaliveInterval, func() { _ = resp.Body.Close() })
		delivery = keepalive
	}
	copyNativeCLIResponseHeaders(delivery.Header(), resp.Header)
	// Keep malformed/non-JSON non-stream responses from committing HTTP 200.
	if keepalive == nil {
		raw, validationErr := readAndValidateNativeResponse(resp.Body)
		_ = resp.Body.Close()
		if validationErr != nil {
			result.Err = validationErr
			writeResponsesAPIError(w, http.StatusBadGateway, "upstream_error", validationErr.Error())
			return
		}
		resp.Body = io.NopCloser(bytes.NewReader(raw))
	}
	func() {
		if keepalive != nil {
			defer func() {
				if err := keepalive.Close(); err != nil && result.Err == nil {
					result.Err, result.Finish = err, "error"
				}
			}()
		}
		defer resp.Body.Close()
		delivery.WriteHeader(resp.StatusCode)
		responseID, captured, result = copyNativeResponse(delivery, resp.Body, resp.Header.Get("Content-Type"), spec.ID, namespaces)
	}()
	h.auditChatOutcome(ctx, sess.acc, req, result)
	return
}
