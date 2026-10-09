package grok

import (
	"encoding/json"
	"io"
	"net/http"
	"orchids-api/internal/responses"

	"orchids-api/internal/middleware"
)

func (h *Handler) HandleResponses(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	// Keep the original object for the Build CLI route.  The CLI upstream
	// speaks Responses natively, so translating it through Chat Completions
	// would drop valid fields such as previous_response_id and metadata.
	body, err := readBoundedJSONBody(w, r)
	if err != nil {
		return
	}
	var req responses.CreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeGrokError(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Model = normalizeModelID(req.Model)
	// Publish the resolved model so the per-minute aggregation can attribute this
	// request to a model; the latency middleware never re-reads the body.
	r = r.WithContext(middleware.WithRequestModel(r.Context(), req.Model))
	if !requireAPIKeyModel(w, r, req.Model) {
		return
	}
	h.applyDefaultResponsesStream(&req)
	var nativePayload map[string]interface{}
	if err := json.Unmarshal(body, &nativePayload); err != nil || nativePayload == nil {
		writeGrokError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if _, provided := nativePayload["stream"]; !provided {
		nativePayload["stream"] = req.Stream
	}
	identityMessages, _ := responses.InputToMessagesMode(req.Input, false)
	session := prepareGrokSession(r, req.Model, req.PromptCacheKey, identityMessages)
	if session.Key != "" {
		nativePayload["prompt_cache_key"] = session.Key
		r = r.WithContext(withGrokSession(r.Context(), session))
		if session.Replay {
			h.applyNativeReasoningReplay(req.Model, session.Key, nativePayload)
		}
	}

	spec, resolved := h.resolveConversationModel(r.Context(), req.Model)
	if !resolved {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", modelNotFoundMessage(req.Model))
		return
	}
	// Every resolvable Grok model uses Build CLI, so the native Responses path
	// handles all of them; other providers use ResponsesChannelBridgeHandler.
	// A compaction turn is not a conversation turn: Codex remote-v2 sends
	// `compaction_trigger` and the Grok TUI appends the canonical summary
	// prompt as its last user item. The gateway answers those itself so the
	// resulting state stays portable across accounts.
	if h.GatewayCompactionEnabled() && responses.ClassifyCompactionPayload(nativePayload) != responses.CompactionNone {
		h.handleGatewayCompaction(w, r, req.Model, spec, nativePayload, responsesPayloadStreaming(nativePayload, req.Stream))
		return
	}
	h.handleNativeCLIResponses(w, r, req.Model, spec, nativePayload)
}

// handleNativeCLIResponses proxies Build OAuth Responses requests without a
// Chat-Completions compatibility conversion.  Besides preserving the official
// request and event schema, this keeps SSE streaming realtime and bounded by
// the normal HTTP backpressure instead of buffering the whole completion.
func (h *Handler) handleNativeCLIResponses(w http.ResponseWriter, r *http.Request, modelID string, spec ModelSpec, payload map[string]interface{}) {
	h.handleNativeCLIResponsesAt(w, r, modelID, spec, payload, "/responses", true)
}

func copyNativeCLIResponseHeaders(dst, src http.Header) {
	// Forward only end-to-end response metadata. Hop-by-hop headers must not be
	// copied because net/http owns the downstream connection.
	for _, key := range []string{"Content-Type", "Cache-Control", "X-Request-Id", "X-Request-ID", "X-Grok2api-Compatibility-Warnings", "X-Grok2api-Reasoning-Recovery"} {
		if values, ok := src[key]; ok {
			dst.Del(key)
			for _, value := range values {
				dst.Add(key, value)
			}
		}
	}
}

func streamNativeCLIResponse(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return
		}
		if err != nil {
			return
		}
	}
}

func (h *Handler) applyDefaultResponsesStream(req *responses.CreateRequest) {
	if req == nil || req.StreamProvided {
		return
	}
	req.Stream = h.defaultChatStream()
}
