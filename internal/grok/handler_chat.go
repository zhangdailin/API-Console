package grok

import (
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/debug"
	"orchids-api/internal/logutil"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

// Chat completions. The only Grok upstream this gateway speaks here is the Build
// (OAuth CLI) plane; the legacy website and developer planes were removed,
// so every conversation model is served by the native Responses bridge.

func (h *Handler) defaultChatStream() bool {
	if h == nil || h.configSnapshot() == nil {
		return true
	}
	return h.configSnapshot().ChatDefaultStream()
}

func (h *Handler) applyDefaultChatStream(req *ChatCompletionsRequest) {
	if req == nil {
		return
	}
	if req.StreamProvided {
		return
	}
	req.Stream = h.defaultChatStream()
}

func (h *Handler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req ChatCompletionsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	req.startedAt = time.Now()
	req.sourceOperation, _ = r.Context().Value(chatSourceOperationKey{}).(string)
	verboseDiagnostics := logutil.VerboseDiagnosticsEnabled()
	debugLogSSE := h != nil && h.configSnapshot() != nil && h.configSnapshot().DebugLogSSE
	logger := debug.NewForContext(r.Context(), verboseDiagnostics, verboseDiagnostics && debugLogSSE)
	defer logger.Close()
	logger.LogIncomingRequest(req)
	req.Model = normalizeModelID(req.Model)
	// Publish the resolved model so the per-minute aggregation can attribute this
	// request to a model: the latency middleware cannot read the body itself.
	r = r.WithContext(middleware.WithRequestModel(r.Context(), req.Model))
	h.applyDefaultChatStream(&req)
	if err := req.Validate(); err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	if !requireAPIKeyModel(w, r, req.Model) {
		return
	}
	session := sessionFromContext(r.Context())
	if session.Key == "" {
		session = prepareGrokSession(r, req.Model, req.PromptCacheKey, req.Messages)
	}
	if session.Key != "" {
		req.PromptCacheKey = session.Key
		req.ReasoningReplay = session.Replay
		r = r.WithContext(withGrokSession(r.Context(), session))
	}
	spec, ok := h.resolveConversationModel(r.Context(), req.Model)
	if !ok {
		writeGrokErrorCode(w, http.StatusNotFound, "model_not_found", modelNotFoundMessage(req.Model))
		return
	}
	if spec.AliasReasoningEffort != "" {
		effort := spec.AliasReasoningEffort
		req.ReasoningEffort = &effort
	}
	if err := h.ensureResolvedModelCapability(r.Context(), spec.ID, spec, store.CapabilityChat); err != nil {
		if strings.EqualFold(strings.TrimSpace(err.Error()), "model not found") {
			writeGrokErrorCode(w, http.StatusNotFound, "model_not_found", modelNotFoundMessage(req.Model))
			return
		}
		writeGrokError(w, http.StatusBadRequest, modelValidationMessage(req.Model, err))
		return
	}
	if !modelRoutedToCLI(spec, h.configSnapshot()) {
		writeGrokErrorCode(w, http.StatusNotFound, "model_not_found", modelNotFoundMessage(req.Model))
		return
	}
	// The request's model travels on the context so account selection can skip an
	// account that is cooling down for this model only; the account's other models
	// stay in the pool.
	r = r.WithContext(WithRequestModel(r.Context(), spec.ID))
	sess, err := h.openCLIAccountSession(r.Context(), nil, spec.UpstreamModel)
	if err != nil {
		writeGrokNoAccountError(w, err)
		return
	}
	defer sess.Close()
	req.account = sess.acc
	h.serveNativeChat(r.Context(), w, &req, spec, sess, logger, true)
}

// debugHeaderMap renders a header set for the debug journal, redacting the
// credential-bearing entries.
func debugHeaderMap(headers http.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for k, values := range headers {
		if isSensitiveUpstreamHeader(k) {
			out[k] = "[redacted]"
			continue
		}
		out[k] = strings.Join(values, ", ")
	}
	return out
}
