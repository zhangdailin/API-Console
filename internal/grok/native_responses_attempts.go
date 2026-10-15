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
	"orchids-api/internal/middleware"
	"orchids-api/internal/responses"
)

func nativeQualityRequest(model string, payload map[string]any) *chatwire.Request {
	req := &chatwire.Request{Model: model, StartedAt: time.Now(), SourceOperation: "responses", ResponsesTools: responses.InterfaceMaps(payload["tools"]), MCPServers: responses.InterfaceMaps(payload["mcp_servers"])}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if effort, ok := reasoning["effort"].(string); ok && strings.TrimSpace(effort) != "" {
		req.ReasoningEffort = &effort
	}
	return req
}

// serveNativeResponses shares one physical-call budget across transport,
// reasoning recovery and quality retries. Every account starts from immutable
// input and rejected output never establishes response ownership or replay.
func (h *Handler) serveNativeResponses(w http.ResponseWriter, r *http.Request, sess *chatAccountSession, pinned bool, spec ModelSpec, payload map[string]any, path string, namespaces responses.ToolNamespaces) (responseID string, captured []byte, result chatOutcome) {
	ctx := r.Context()
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
		response, failure := h.buildClient().doResponsesAt(withReasoningDiagnostics(ctx, payload), sess.acc, path, payload)
		if stage != "" {
			h.auditAttemptDiagnostic(ctx, sess.acc, ProviderBuild, physical, started, failure, stage, response, nil, "")
		}
		return response, failure
	}
	request := func() (*http.Response, error) {
		if err := prepareBuildPayload(immutable, payload, sess.acc, spec.UpstreamModel); err != nil {
			return nil, err
		}
		ctx = withReasoningDiagnostics(ctx, payload)
		req = nativeQualityRequest(spec.ID, payload)
		initialStarted := time.Now()
		response, failure := oneCall("")
		if failure == nil || !isReasoningReplayDecodeError(failure) || preservesClientCompaction(payload, failure) || remaining <= 0 {
			return response, failure
		}
		h.auditAttemptDiagnostic(ctx, sess.acc, ProviderBuild, physical, initialStarted, failure, "reasoning_decode_rejected", response, nil, "")
		if session := sessionFromContext(ctx); session.Key != "" {
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
	policy := h.qualityHoldPolicy()
	for attempt := 1; ; attempt++ {
		// Hosted tools may already have executed unless the upstream explicitly
		// rejected the free allowance before generation. Continuations always
		// remain on their creating account, including after such a rejection.
		unsafe := pinned || chatwire.ParseLooseStringAny(immutable["previous_response_id"]) != "" || qualityRequestReplayUnsafe(nativeQualityRequest(spec.ID, immutable))
		limit := remaining
		var allowSwitch func(error) bool
		if pinned || chatwire.ParseLooseStringAny(immutable["previous_response_id"]) != "" {
			limit = 1
		} else if unsafe {
			allowSwitch = isFreeUsageExhaustedError
		}
		resp, failure := h.retryWithAccountSwitchWhen(ctx, sess, 1500*time.Millisecond, request, openNext, nil, limit, allowSwitch)
		if failure != nil {
			result.Err = failure
			writeGrokUpstreamFailure(w, upstreamHTTPResponseStatus(failure), failure)
			return
		}
		h.syncGrokQuota(sess.acc, resp.Header)
		var quality *nativeResponseQuality
		var target http.ResponseWriter = w
		if path == "/responses" && responses.ClassifyCompactionPayload(immutable) == responses.CompactionNone && h.shouldHoldQualityTurn(req, ProviderBuild) {
			quality = newNativeResponseQuality(w, policy)
			target = quality.hold.writer
			h.auditQualityCheck(ctx, sess.acc, req, "checking", "native", attempt, req.StartedAt, chatOutcome{})
		}
		copyNativeCLIResponseHeaders(target.Header(), resp.Header)
		// Keep malformed/non-JSON non-stream responses from committing HTTP 200.
		if quality == nil && !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
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
			defer resp.Body.Close()
			target.WriteHeader(resp.StatusCode)
			responseID, captured, result = copyNativeResponseWithQuality(target, resp.Body, resp.Header.Get("Content-Type"), spec.ID, namespaces, quality)
		}()
		if quality == nil {
			h.auditChatOutcome(ctx, sess.acc, req, result)
			return
		}
		result.QualityChecked, result.QualityAttempt = true, attempt
		result.Quality.Terminal = result.Err == nil
		guard := h.applyBuildQualityGuard(ctx, sess.acc, req, result)
		if !result.Withheld {
			if result.Err == nil {
				result.Err = quality.hold.writer.Commit()
			}
			status := "passed"
			if result.Err != nil {
				status = "error"
				if quality.hold.writer.Committed() {
					middleware.MarkStreamFailure(w)
				} else {
					quality.hold.writer.Discard()
					writeResponsesAPIError(w, http.StatusBadGateway, "upstream_error", "Upstream response could not be safely delivered")
				}
			}
			h.auditQualityCheck(ctx, sess.acc, req, status, "native", attempt, req.StartedAt, result)
			h.auditChatOutcome(ctx, sess.acc, req, result)
			return
		}
		quality.hold.writer.Discard()
		h.auditChatOutcome(ctx, sess.acc, req, result)
		h.auditQualityDegraded(ctx, sess.acc, req, result, qualityWithheldTurn{Mode: "native", Attempt: attempt, Guard: guard})
		if unsafe || remaining <= 0 || attempt >= policy.MaxAttempts || ctx.Err() != nil {
			result.Err = errQualityWithheld
			writeResponsesAPIError(w, http.StatusBadGateway, "quality_degraded", "Upstream response failed the reasoning quality check")
			return
		}
		previous := sess.acc
		sess.Close()
		next, switchErr := openNext(used)
		if switchErr != nil {
			result.Err = errQualityWithheld
			writeResponsesAPIError(w, http.StatusBadGateway, "quality_degraded", "No account could provide a response that passed the reasoning quality check")
			return
		}
		sess.acc, sess.token, sess.poolCandidates, sess.release = next.acc, next.token, next.poolCandidates, next.release
		h.auditQualityRetry(ctx, previous, sess.acc, req, attempt+1)
	}
}
