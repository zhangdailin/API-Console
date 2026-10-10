package grok

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"orchids-api/internal/util"
)

func copyNativeCLIResponseAndCaptureModel(w http.ResponseWriter, body io.Reader, contentType, model string, mappings ...responses.ToolNamespaces) (responseID string, captured []byte, result chatOutcome) {
	return copyNativeResponseWithVisibility(w, body, contentType, model, false, mappings...)
}

func copyNativeResponseWithVisibility(w http.ResponseWriter, body io.Reader, contentType, model string, hideReasoning bool, mappings ...responses.ToolNamespaces) (responseID string, captured []byte, result chatOutcome) {
	var namespaces responses.ToolNamespaces
	if len(mappings) > 0 {
		namespaces = mappings[0]
	}
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
		responseID = chatwire.ParseLooseStringAny(response["id"])
		result.Usage = consoleUsage(response)
		if len(result.Usage) > 0 {
			result.UsageSource = audit.UsageSourceUpstream
		}
		result.Finish, result.Err = responses.TerminalFinish("", response)
		// Preserve JSON bytes unless restoring tool identities or masking errors.
		// Partial SSE events also need strict-client supplementation below.
		changed := namespaces.RestoreEnvelope(response)
		if redactResponseError(response) || changed {
			raw, _ = json.Marshal(response)
		}
		_, _ = fullCapture.Write(raw)
		if hideReasoning && hideNativeReasoning(response) {
			raw, _ = json.Marshal(response)
		}
		written, writeErr := w.Write(raw)
		result.Err = writeErr
		if written != len(raw) && result.Err == nil {
			result.Err = io.ErrShortWrite
		}
		return
	}

	flusher, _ := w.(http.Flusher)
	target := deadlineResponseWriter{ResponseWriter: w}
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
			kind = util.FirstNonEmpty(chatwire.ParseLooseStringAny(event["type"]), frame.Event)
			// response.doom_loop_check is a private Grok control event: it is
			// not part of the Responses schema, and a strict client (Codex,
			// Grok TUI) treats an unknown type as a protocol error. It never
			// crosses the public boundary: it is filtered here.
			if responses.IsPrivateBuildControlEvent(kind) {
				return nil
			}
			response, _ := event["response"].(map[string]interface{})
			if id := chatwire.ParseLooseStringAny(response["id"]); id != "" {
				responseID = id
			}

		}
		changed := namespaces.RestoreEnvelope(event)
		changed = supplementResponsesEvent(event, compat) || changed
		if redactResponseError(event) || changed {
			raw, _ := json.Marshal(event)
			// The compat layer changed the payload, so the frame cannot be relayed
			// as the upstream sent it.
			frame.SetData(string(raw))
		}
		// Capture the unfiltered protocol for private replay; the client receives
		// only the selected visibility projection.
		_ = frame.WriteFrame(fullCapture)
		if !hideReasoning || !hiddenReasoningEvent(kind, event) {
			if hideReasoning && hideNativeReasoning(event) {
				raw, _ := json.Marshal(event)
				frame.SetData(string(raw))
			}
			if err := frame.WriteFrame(target); err != nil {
				result.Err = err
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if usage := consoleUsageFromStreamEvent(event); len(usage) > 0 {
			result.Usage = usage
			result.UsageSource = audit.UsageSourceUpstream
		}
		item, _ := event["item"].(map[string]interface{})
		meaningful := strings.HasSuffix(kind, ".delta") && responses.StreamString(event["delta"]) != ""
		toolStart := kind == "response.output_item.added" && chatwire.ParseLooseStringAny(item["type"]) == "function_call" && chatwire.ParseLooseStringAny(item["name"]) != ""
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
