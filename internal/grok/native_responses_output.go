package grok

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/responses"
	"orchids-api/internal/util"
)

func copyNativeResponseWithQuality(w http.ResponseWriter, body io.Reader, contentType, model string, namespaces responses.ToolNamespaces, quality *nativeResponseQuality) (responseID string, captured []byte, result chatOutcome) {
	result.Quality.FirstVisibleMS = -1
	if quality != nil {
		quality.buffered = applyToolResponseTiming(body, &result.Quality)
	}
	// Keep the complete delivered response for replay and usage recovery.
	fullCapture := new(bytes.Buffer)
	defer func() {
		captured = fullCapture.Bytes()
		if result.Err != nil {
			result.Finish = "error"
		}
	}()
	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		raw, readErr := io.ReadAll(body)
		if readErr != nil {
			result.Err = fmt.Errorf("upstream response could not be read: %w", readErr)
		}
		if result.Err != nil {
			writeResponsesAPIError(w, http.StatusBadGateway, "upstream_error", "Upstream response unavailable")
			return
		}
		var response map[string]any
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
		if quality != nil && result.Err == nil {
			quality.observeResponse(response, &result)
			if err := quality.check(true); err != nil {
				result.Err = err
				return
			}
			if result.Withheld {
				return
			}
		}
		// Preserve JSON bytes unless restoring tool identities or masking errors.
		// Partial SSE events also need strict-client supplementation below.
		changed := namespaces.RestoreEnvelope(response)
		if redactResponseError(response) || changed {
			raw, _ = json.Marshal(response)
		}
		_, _ = fullCapture.Write(raw)
		written, writeErr := w.Write(raw)
		if writeErr == nil && written != len(raw) {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			result.Err = writeErr
		}
		return
	}

	target := &httpserver.CheckedStreamWriter{Target: deadlineResponseWriter{ResponseWriter: w}}
	terminal, done := false, false
	failureCode, failureMessage := "", ""
	compat := &responsesCompatibilityState{model: model}
	err := responses.ConsumeSSE(body, func(frame responses.SSEEvent) error {
		var event map[string]any
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
			response, _ := event["response"].(map[string]any)
			if id := chatwire.ParseLooseStringAny(response["id"]); id != "" {
				responseID = id
			}

		}
		quality.observe(event, kind, &result)
		changed := namespaces.RestoreEnvelope(event)
		changed = supplementResponsesEvent(event, compat) || changed
		if redactResponseError(event) || changed {
			raw, _ := json.Marshal(event)
			// The compat layer changed the payload, so the frame cannot be relayed
			// as the upstream sent it.
			frame.SetData(string(raw))
		}
		// Capture the same protocol sent to the client for private replay.
		_ = frame.WriteFrame(fullCapture)
		if err := frame.WriteFrame(target); err != nil {
			result.Err = err
			return err
		}
		target.Flush()
		if target.Err != nil {
			result.Err = target.Err
			return target.Err
		}
		if usage := consoleUsageFromStreamEvent(event); len(usage) > 0 {
			result.Usage = usage
			result.UsageSource = audit.UsageSourceUpstream
		}
		item, _ := event["item"].(map[string]any)
		meaningful := strings.HasSuffix(kind, ".delta") && responses.StreamString(event["delta"]) != ""
		toolStart := kind == "response.output_item.added" && chatwire.ParseLooseStringAny(item["type"]) == "function_call" && chatwire.ParseLooseStringAny(item["name"]) != ""
		if result.FirstToken.IsZero() && (meaningful || toolStart) {
			result.FirstToken = time.Now()
		}
		switch kind {
		case "response.completed", "response.failed", "response.incomplete", "error":
			response, _ := event["response"].(map[string]any)
			if response == nil {
				response = event
			}
			result.Finish, result.Err = responses.TerminalFinish(kind, response)
			terminal = true
			if result.Err == nil && quality != nil {
				if err := quality.check(true); err != nil {
					result.Err = err
				}
			}
			return io.EOF // A logical terminal must not wait for the socket to close.
		}
		if quality != nil {
			if err := quality.check(false); err != nil {
				result.Err = err
				return err
			}
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
		failure, _ := json.Marshal(map[string]any{
			"type": "response.failed", "response": map[string]any{
				"id": responseID, "object": "response", "status": "failed", "model": model,
				"created_at": now, "completed_at": now,
				"output": []any{},
				"error":  map[string]any{"code": failureCode, "message": failureMessage},
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
	target.Flush()
	if target.Err != nil {
		result.Err = target.Err
	}
	return
}
