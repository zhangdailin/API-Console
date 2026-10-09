package grok

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"strconv"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/audit"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

// maxNativeResponsesBytes bounds a buffered non-streaming native Build
// Responses body. The reference implementation allows 128 MiB; the previous
// 8 MiB rejected a long reasoning turn as an upstream fault, and the client
// then retried a request that had already succeeded upstream.
const maxNativeResponsesBytes = 128 << 20

func (h *Handler) handleNativeCLIResponsesAt(w http.ResponseWriter, r *http.Request, modelID string, spec ModelSpec, payload map[string]interface{}, upstreamPath string, saveOwnership bool) {
	spec.Upstream = UpstreamCLI
	// Native Responses bypasses the chat handler that normally installs Grok's
	// model context. Keep the upstream model on every selection/request/retry so a
	// model-scoped Free refusal cannot be escalated into a 24-hour account block.
	r = r.WithContext(WithRequestModel(r.Context(), spec.UpstreamModel))
	started := time.Now()
	if err := validatePayloadReasoning(payload); err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// Gateway-owned compaction state is expanded before the payload is
	// normalized, so the summary reaches the upstream as an ordinary user
	// message and the reasoning-replay machinery never sees a sealed blob.
	if codec := h.compactionCodecSnapshot(); codec.Available() {
		drifted, expandErr := responses.ExpandCompactionHistory(payload, codec, sessionFromContext(r.Context()).Key)
		if expandErr != nil {
			writeResponsesAPIErrorWithParam(w, http.StatusBadRequest, "invalid_compaction_blob", expandErr.Error(), compactionErrorParam(expandErr))
			return
		}
		if drifted > 0 {
			w.Header().Set("X-Grok2API-Compaction-Session-Drift", strconv.Itoa(drifted))
		}
	}
	if err := normalizeBuildResponsesPayload(payload); err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := h.ensureModelCapability(r.Context(), modelID, store.CapabilityResponses); err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", modelValidationMessage(modelID, err))
		return
	}
	if h == nil || h.buildClient() == nil {
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "grok cli client not configured")
		return
	}

	ownerHash := middleware.APIKeyFingerprint(r.Context())
	previousID := strings.TrimSpace(chatwire.ParseLooseStringAny(payload["previous_response_id"]))
	var (
		sess   *chatAccountSession
		pinned bool
		err    error
	)
	if previousID != "" && ownerHash != "" {
		ownership, lookupErr := h.getStoredResponse(r, previousID, ownerHash)
		if lookupErr != nil {
			responses.WriteStoredLookupError(w, lookupErr, "previous response not found")
			return
		}
		if ownership.Provider != ProviderBuild {
			writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", "previous response provider is incompatible")
			return
		}
		sess, err = h.openCLIAccountSessionByID(r.Context(), ownership.AccountID, spec.UpstreamModel)
		pinned = true
	} else {
		sess, err = h.openCLIAccountSession(r.Context(), nil, spec.UpstreamModel)
	}
	if err != nil {
		writeGrokAccountUnavailable(w, err, "response_account_unavailable", grokResponseAccountUnavailableMessage)
		return
	}
	defer sess.Close()

	payload["model"] = spec.UpstreamModel
	call := func() (*http.Response, error) {
		if pinned {
			return h.buildClient().doResponsesAt(r.Context(), sess.acc, upstreamPath, payload)
		}
		return h.doCLIWithAutoSwitchAt(r.Context(), sess, payload, spec.UpstreamModel, upstreamPath)
	}
	resp, err := call()
	if err != nil && isReasoningReplayDecodeError(err) && !preservesClientCompaction(payload, err) {
		// Upstream rejected opaque reasoning it could not decode. Recovery stays
		// on the same account and plane: drop only the undecodable ciphers while
		// keeping readable summaries, and clear the server-side replay first so
		// the same stale cipher is never injected again. Client-held compaction
		// state is never rewritten.
		if session := sessionFromContext(r.Context()); session.Key != "" {
			h.clearReasoningReplay(r.Context(), modelID, session.Key)
		}
		if stripInjectedReasoningReplay(payload) {
			retryResp, retryErr := call()
			if retryErr == nil && retryResp != nil {
				retryResp.Header.Set("X-Grok2API-Reasoning-Recovery", "reasoning_encrypted_content_retry")
			}
			resp, err = retryResp, retryErr
		}
	}
	if err != nil {
		if markAllGrokAccountStatuses(err) {
			h.markAccountStatus(r.Context(), sess.acc, err)
		}
		writeGrokUpstreamFailure(w, upstreamHTTPResponseStatus(err), err)
		return
	}
	defer resp.Body.Close()
	h.syncGrokQuota(sess.acc, resp.Header)
	// A non-streaming body is validated before the upstream status is committed;
	// otherwise malformed JSON locks the client into a misleading 200.
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		raw, validationErr := readAndValidateNativeResponse(resp.Body)
		if validationErr != nil {
			writeResponsesAPIError(w, http.StatusBadGateway, "upstream_error", validationErr.Error())
			return
		}
		resp.Body = io.NopCloser(strings.NewReader(string(raw)))
	}
	copyNativeCLIResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	responseID, captured, result := copyNativeCLIResponseAndCaptureModel(w, resp.Body, resp.Header.Get("Content-Type"), modelID)
	h.auditChatOutcome(r.Context(), sess.acc, &chatwire.Request{Model: modelID, StartedAt: started}, result)
	if session := sessionFromContext(r.Context()); session.Replay && len(captured) > 0 && result.Err == nil {
		h.captureReasoningReplay(r.Context(), modelID, session.Key, captured)
	}

	if !saveOwnership || ownerHash == "" || responseID == "" || resp.StatusCode < 200 || resp.StatusCode >= 300 || result.Err != nil {
		return
	}
	// A continuation request only carries this turn's input; the history lives in
	// the response it names. The stored record is what GET input_items answers
	// from, so the chain is folded in here — otherwise a client that continues a
	// conversation reads back a list with only the last turn in it.
	storedInput := responses.InputItemsJSON(h.accumulatedInputItems(r, ownerHash, payload))
	if err := h.saveStoredResponse(r, &store.StoredResponse{
		ResponseID: responseID,
		OwnerHash:  ownerHash,
		AccountID:  sess.acc.ID,
		Model:      spec.UpstreamModel,
		Provider:   ProviderBuild,
		// Build keeps the response body upstream, so the input items are the only
		// part of the exchange the gateway can serve back itself. Persisting them
		// lets GET /responses/{id}/input_items answer locally instead of doing a
		// second upstream round trip for data it already had.
		InputItems:         storedInput,
		PreviousResponseID: strings.TrimSpace(chatwire.ParseLooseStringAny(payload["previous_response_id"])),
	}); err != nil {
		slog.Error("failed to save response ownership", "response_id", responseID, "account_id", sess.acc.ID, "error", err)
	}
}

// HandleResponsesCompact forwards the native Build Responses compaction API.
// Compaction is deliberately non-streaming and is not stored as a normal
// response resource.
func (h *Handler) HandleResponsesCompact(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var payload map[string]interface{}
	if !decodeJSONBody(w, r, &payload) || payload == nil {
		return
	}
	modelID := normalizeModelID(chatwire.ParseLooseStringAny(payload["model"]))
	if modelID == "" {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if !requireAPIKeyModel(w, r, modelID) {
		return
	}
	spec, ok := h.resolveConversationModel(r.Context(), modelID)
	if !ok {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", "responses compact requires a Grok Build model")
		return
	}
	// The whole point of this endpoint is compaction, so it takes the gateway
	// path whenever the gateway can own the summary. The upstream blob a pure
	// forward returns is readable only by the account that produced it, which is
	// exactly what breaks a continuation served by another account. Compaction
	// is never streamed.
	payload["stream"] = false
	if h.GatewayCompactionEnabled() {
		h.handleGatewayCompaction(w, r, modelID, spec, payload, false)
		return
	}
	h.handleNativeCLIResponsesAt(w, r, modelID, spec, payload, "/responses/compact", false)
}

// HandleResponseResource retrieves or deletes a stored Build Responses
// resource through the exact account that created it.
//
// The sibling endpoints below a response id are dispatched first: they are
// resources of a response, so the id parser must never mistake the trailing
// action for part of the id.
func (h *Handler) HandleResponseResource(w http.ResponseWriter, r *http.Request) {
	if action := responses.SubResourceAction(r.URL.Path); action != "" {
		responses.SubResourceHandler(action, h.bridgeOptions())(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "GET, DELETE")
		writeResponsesAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	responseID := responses.ResponseIDFromResourcePath(r.URL.Path)
	if responseID == "" {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", "response_id is required")
		return
	}
	ownerHash := responses.OwnerHash(r.Context())
	ownership, err := h.getStoredResponse(r, responseID, ownerHash)
	if err != nil {
		responses.WriteStoredLookupError(w, err, "response not found")
		return
	}
	if ownership.Provider != ProviderBuild {
		responses.ServeStoredResource(w, r, ownership, h.lb.Store)
		return
	}
	sess, err := h.openCLIAccountSessionByID(r.Context(), ownership.AccountID, ownership.Model)
	if err != nil {
		writeGrokAccountUnavailable(w, err, "response_account_unavailable", grokResponseAccountUnavailableMessage)
		return
	}
	defer sess.Close()

	path := "/responses/" + url.PathEscape(responseID)
	resp, err := h.buildClient().doResponseResource(r.Context(), sess.acc, r.Method, path, r.URL.RawQuery)
	if err != nil {
		writeGrokUpstreamFailure(w, http.StatusBadGateway, err)
		return
	}
	defer resp.Body.Close()
	copyNativeCLIResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	streamNativeCLIResponse(w, resp.Body)
	if (r.Method == http.MethodDelete && resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		_ = h.deleteStoredResponse(r, responseID, ownerHash)
	}
}

// maxStoredInputChainDepth bounds how many previous responses are folded into a
// stored input list, so a long conversation cannot make one record unbounded.
const maxStoredInputChainDepth = 8

// accumulatedInputItems returns this turn's input followed by the input of the
// responses it continues (nearest ancestor first, bounded).
func (h *Handler) accumulatedInputItems(r *http.Request, ownerHash string, payload map[string]interface{}) []interface{} {
	current := responses.InputItems(payload["input"])
	if h == nil || r == nil || ownerHash == "" {
		return current
	}
	previousID := strings.TrimSpace(chatwire.ParseLooseStringAny(payload["previous_response_id"]))
	seen := map[string]bool{}
	for depth := 0; depth < maxStoredInputChainDepth && previousID != "" && !seen[previousID]; depth++ {
		seen[previousID] = true
		record, err := h.getStoredResponse(r, previousID, ownerHash)
		if err != nil || record == nil {
			break
		}
		if len(record.InputItems) > 0 {
			var decoded []interface{}
			if json.Unmarshal(record.InputItems, &decoded) == nil {
				current = append(current, decoded...)
			}
		}
		previousID = strings.TrimSpace(record.PreviousResponseID)
	}
	return current
}

func (h *Handler) getStoredResponse(r *http.Request, responseID, ownerHash string) (*store.StoredResponse, error) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return nil, errors.New("response store not configured")
	}
	return h.lb.Store.GetStoredResponse(r.Context(), responseID, ownerHash)
}

// bridgeOptions describes the response store the shared Responses helpers should
// use for this handler.
//
// The native handler and the channel bridge are wired to the same store, so
// handing the native path the bridge's options lets one implementation of cancel
// and input_items serve records written by either of them.
func (h *Handler) bridgeOptions() responses.BridgeOptions {
	opts := responses.BridgeOptions{}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return opts
	}
	opts.Store = h.lb.Store
	if cfg := h.configSnapshot(); cfg != nil && cfg.ResponseStoreTTL > 0 {
		opts.TTL = time.Duration(cfg.ResponseStoreTTL) * time.Hour
	}
	return opts
}

func (h *Handler) saveStoredResponse(r *http.Request, response *store.StoredResponse) error {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return errors.New("response store not configured")
	}
	return h.lb.Store.SaveStoredResponse(r.Context(), response, h.bridgeOptions().TTLOrDefault())
}

func (h *Handler) deleteStoredResponse(r *http.Request, responseID, ownerHash string) error {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return errors.New("response store not configured")
	}
	return h.lb.Store.DeleteStoredResponse(r.Context(), responseID, ownerHash)
}

func readAndValidateNativeResponse(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxNativeResponsesBytes+1))
	if err != nil || len(raw) > maxNativeResponsesBytes {
		return nil, fmt.Errorf("Upstream response unavailable")
	}
	var response map[string]interface{}
	if json.Unmarshal(raw, &response) != nil || response == nil {
		return nil, fmt.Errorf("Invalid upstream response")
	}
	return raw, nil
}

func copyNativeCLIResponseAndCaptureModel(w http.ResponseWriter, body io.Reader, contentType, model string) (responseID string, captured []byte, result chatOutcome) {
	// The streaming capture is a bounded side buffer for usage/model recovery;
	// only the non-streaming body needs the larger ceiling.
	fullCapture := newBoundedResponseCapture(8 << 20)
	defer func() {
		captured = fullCapture.data
		if result.Err != nil {
			result.Finish = "error"
		}
	}()
	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		raw, readErr := io.ReadAll(io.LimitReader(body, maxNativeResponsesBytes+1))
		if readErr != nil {
			result.Err = fmt.Errorf("upstream response could not be read within the response limit: %w", readErr)
		} else if len(raw) > maxNativeResponsesBytes {
			result.Err = fmt.Errorf("upstream response could not be read within the response limit")
		}
		if result.Err != nil {
			writeResponsesAPIError(w, http.StatusBadGateway, "upstream_error", "Upstream response unavailable")
			return
		}
		var response map[string]interface{}
		if result.Err = json.Unmarshal(raw, &response); result.Err != nil {
			writeResponsesAPIError(w, http.StatusBadGateway, "upstream_error", "Invalid upstream response")
			return
		}
		if response == nil {
			result.Err = fmt.Errorf("invalid upstream response JSON")
			return
		}
		responseID = interfaceString(response["id"])
		result.Usage = consoleUsage(response)
		if len(result.Usage) > 0 {
			result.UsageSource = audit.UsageSourceUpstream
		}
		result.Finish, result.Err = responses.TerminalFinish("", response)
		// Whole JSON Responses objects are already self-contained. Preserve their
		// original bytes; strict serde supplementation is only needed for partial
		// SSE events that clients assemble incrementally.
		if redactResponseError(response) {
			raw, _ = json.Marshal(response)
		}
		_, _ = io.MultiWriter(w, fullCapture).Write(raw)
		return
	}

	flusher, _ := w.(http.Flusher)
	target := io.MultiWriter(deadlineResponseWriter{ResponseWriter: w}, fullCapture)
	terminal, done := false, false
	failureCode, failureMessage := "", ""
	compat := &responsesCompatibilityState{model: model}
	err := responses.ConsumeSSE(body, func(frame responses.SSEEvent) error {
		var event map[string]interface{}
		kind := ""
		if frame.HasData() {
			data := frame.Data()
			if string(data) == "[DONE]" {
				done = true
				return io.EOF
			}
			if err := json.Unmarshal(data, &event); err != nil || event == nil {
				failureCode, failureMessage = "invalid_upstream_event", "upstream response event is not a JSON object"
				return fmt.Errorf("%s", failureMessage)
			}
			kind = firstNonEmpty(interfaceString(event["type"]), frame.Event)
			// response.doom_loop_check is a private Grok control event: it is
			// not part of the Responses schema, and a strict client (Codex,
			// Grok TUI) treats an unknown type as a protocol error. It never
			// crosses the public boundary: it is filtered here.
			if responses.IsPrivateBuildControlEvent(kind) {
				return nil
			}
			response, _ := event["response"].(map[string]interface{})
			if id := interfaceString(response["id"]); id != "" {
				responseID = id
			}

		}
		if supplementResponsesEvent(event, compat) || redactResponseError(event) {
			raw, _ := json.Marshal(event)
			// The compat layer changed the payload, so the frame cannot be relayed
			// as the upstream sent it.
			frame.SetData(string(raw))
		}
		if err := frame.WriteFrame(target); err != nil {
			result.Err = err
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		if usage := consoleUsageFromStreamEvent(event); len(usage) > 0 {
			result.Usage = usage
			result.UsageSource = audit.UsageSourceUpstream
		}
		item, _ := event["item"].(map[string]interface{})
		meaningful := strings.HasSuffix(kind, ".delta") && streamString(event["delta"]) != ""
		toolStart := kind == "response.output_item.added" && interfaceString(item["type"]) == "function_call" && interfaceString(item["name"]) != ""
		if result.FirstToken.IsZero() && (meaningful || toolStart) {
			result.FirstToken = time.Now()
		}
		switch kind {
		case "response.completed", "response.failed", "response.incomplete", "error":
			response, _ := event["response"].(map[string]interface{})
			if response == nil {
				response = event
			}
			result.Finish, result.Err = responses.TerminalFinish(kind, response)
			terminal = true
			return io.EOF // A logical terminal must not wait for the socket to close.
		}
		return nil
	})
	if !terminal && result.Err != nil {
		return
	}
	if !terminal {
		if failureCode == "" {
			failureCode, failureMessage = "upstream_stream_incomplete", "upstream stream ended before a terminal response event"
			if err != nil && err != io.EOF {
				failureCode, failureMessage = "stream_read_error", "upstream response stream could not be read"
			} else if done {
				failureCode, failureMessage = "upstream_terminal_missing", "upstream sent [DONE] without a terminal response event"
			}
		}
		// An upstream that rejects the model or the request parameters must not be
		// reported as a transport wobble: the client would retry a request that can
		// never succeed. `err` is the protocol error on this path, and the
		// recorded failure message is the fallback classification input.
		failureCode, failureMessage = classifySynthesizedFailure(failureCode, failureMessage, err)
		result.Err = fmt.Errorf("%s", failureMessage)
		if err != nil && err != io.EOF {
			result.Err = fmt.Errorf("%s: %w", failureMessage, err)
		}
		now := time.Now().Unix()
		failure, _ := json.Marshal(map[string]interface{}{
			"type": "response.failed", "response": map[string]interface{}{
				"id": responseID, "object": "response", "status": "failed", "model": model,
				"created_at": now, "completed_at": now,
				"output": []interface{}{},
				"error":  map[string]interface{}{"code": failureCode, "message": failureMessage},
			},
		})
		frame := responses.SSEEvent{Event: "response.failed"}
		frame.SetData(string(failure))
		if err := frame.WriteFrame(target); err != nil {
			result.Err = err
			return
		}
	}
	// No trailing [DONE]: the Responses protocol has no such frame, and the relay
	// relays the native stream as the upstream ends it. A strict serde client
	// treats an unknown frame as a protocol error, and appending one made the two
	// gateways produce different bytes for the same upstream stream.
	if flusher != nil {
		flusher.Flush()
	}
	return
}

// boundedResponseCapture keeps at most limit bytes of a stream, so reasoning
// replay can still be captured when the full body is too large to buffer. Every
// write is reported complete, so the bound never truncates the caller's stream.
type boundedResponseCapture struct {
	data  []byte
	limit int
}

func newBoundedResponseCapture(limit int) *boundedResponseCapture {
	return &boundedResponseCapture{limit: limit, data: make([]byte, 0, min(limit, 64*1024))}
}

func (c *boundedResponseCapture) Write(p []byte) (int, error) {
	if remaining := c.limit - len(c.data); remaining > 0 {
		c.data = append(c.data, p[:min(len(p), remaining)]...)
	}
	return len(p), nil
}

func writeResponsesAPIError(w http.ResponseWriter, status int, code, message string) {
	writeResponsesAPIErrorWithParam(w, status, code, message, "")
}

func writeResponsesAPIErrorWithParam(w http.ResponseWriter, status int, code, message, param string) {
	responses.WriteAPIErrorWithParam(w, status, code, message, param)
}

// upstreamRejectionCode is the code a caller can act on: the upstream refused
// the request itself, so retrying or resending cannot change the outcome.
const upstreamRejectionCode = "upstream_rejection"

// upstreamRejectionMessage is the code-less envelope used when a redaction has
// no error text left to classify (the event carried an empty error object).
const upstreamRejectionMessage = "upstream_rejection"

// Only protocol error envelopes are rewritten; model output and tool arguments
// remain byte-for-byte unchanged on successful events.
//
// A rejection is redacted differently from a failure. "the upstream refused this
// model or these parameters" and "the upstream wobbled" are not the same
// problem, and flattening both onto one code and one message is what leaves a
// client retrying a request that can never succeed. The category decides: a
// client-category error keeps the stable upstream_rejection code and the
// category's public text, everything else keeps the generic upstream_error.
func redactResponseError(event map[string]interface{}) bool {
	if event == nil {
		return false
	}
	changed := false
	// Responses uses error: null on healthy envelopes. Only actual error
	// values need redaction; treating null as an empty error invents a rejection.
	if raw, exists := event["error"]; exists && raw != nil {
		code, message := redactedUpstreamError(raw)
		event["error"] = map[string]interface{}{"code": code, "message": message}
		changed = true
	}
	if event["type"] == "error" {
		code, message := redactedUpstreamError(event["message"])
		for _, key := range []string{"message", "detail", "code", "param"} {
			delete(event, key)
		}
		event["code"] = code
		event["message"] = message
		changed = true
	}
	// A response envelope carries its own status; reconciling the error with it
	// gives the redacted text the same shape as a failure the gateway detected
	// itself (as in writeResponsesStreamFailure), so the two paths agree.
	if response, ok := event["response"].(map[string]interface{}); ok {
		changed = redactResponseError(response) || changed
		reconcileResponseErrorEnvelope(response)
	}
	return changed
}

// redactedUpstreamError classifies the raw error value and returns the code and
// the public message that belong together.
func redactedUpstreamError(raw interface{}) (code string, message string) {
	// An error object reached the client unredacted; its text is still upstream
	// text, so the classification runs on the original before it is dropped.
	text := strings.TrimSpace(upstreamErrorText(raw))
	if text == "" {
		return upstreamRejectionCode, upstreamRejectionMessage
	}
	return codeForCategory(apperrors.ClassifyUpstreamError(text).Category), apperrors.PublicMessage(text)
}

// upstreamErrorText renders the message text of an error value without falling
// back to Go's map formatting, which would fold a structured error into a
// "map[...]" string that no classifier recognises.
func upstreamErrorText(raw interface{}) string {
	switch value := raw.(type) {
	case nil:
		return ""
	case string:
		return value
	case map[string]interface{}:
		return firstNonEmpty(
			interfaceString(value["message"]),
			interfaceString(value["detail"]),
			interfaceString(value["error"]),
			interfaceString(value["code"]),
		)
	default:
		return fmt.Sprint(raw)
	}
}

// codeForCategory maps a classification onto the stable code a client sees. Only
// a rejection gets its own code; every other category keeps the generic one, so
// the already-published upstream_error contract is unchanged for them.
func codeForCategory(category string) string {
	if category == "client" {
		return upstreamRejectionCode
	}
	return "upstream_error"
}

// reconcileResponseErrorEnvelope keeps a failed response envelope consistent:
// the status stays failed and the error message matches the event that carried
// it. Statuses other than "failed" are left untouched, so a completed response
// that merely mentions an error field is never rewritten into a failure.
//
// The message itself is never rewritten here: it was written by the redaction
// that produced the envelope, and re-deriving it from the already-masked text
// would read the generic wrapper "Upstream request failed" instead of the
// original upstream detail.
func reconcileResponseErrorEnvelope(response map[string]interface{}) {
	if response == nil || !strings.EqualFold(interfaceString(response["status"]), "failed") {
		return
	}
	error_, _ := response["error"].(map[string]interface{})
	if error_ == nil || strings.TrimSpace(interfaceString(error_["message"])) == "" || strings.TrimSpace(interfaceString(error_["code"])) == "" {
		return
	}
	response["error"] = map[string]interface{}{
		"code":    interfaceString(error_["code"]),
		"message": interfaceString(error_["message"]),
	}
}

// classifySynthesizedFailure upgrades a gateway-synthesized failure to a
// rejection when the underlying protocol error says the upstream refused the
// request. `err` carries the upstream's own words, while the failure message is
// the gateway's paraphrase, so the error is classified first.
func classifySynthesizedFailure(code, message string, err error) (string, string) {
	// An idle timeout is its own condition: a client has to be able to tell
	// "the upstream stalled" from "the stream was malformed", and the Responses
	// plane has a dedicated code for it: upstream_stream_idle_timeout, rather
	// than a generic read error.
	if err != nil && errors.Is(err, ErrGrokSemanticIdle) {
		return "upstream_stream_idle_timeout", "upstream stream timed out while waiting for generated output"
	}
	if code == "" {
		return code, message
	}
	text := strings.TrimSpace(message)
	if err != nil && err != io.EOF {
		text = strings.TrimSpace(err.Error())
	}
	if text == "" || apperrors.ClassifyUpstreamError(text).Category != "client" {
		return code, message
	}
	return codeForCategory("client"), apperrors.PublicMessage(text)
}
