package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"orchids-api/internal/responses"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

const (
	toolResponseMemoryLimit = 1 << 20
	toolResponseSizeLimit   = 64 << 20
)

type toolArgumentsError struct {
	name   string
	reason error
}

func (e *toolArgumentsError) Error() string {
	return fmt.Sprintf("upstream function %q arguments violate the declared schema: %v", e.name, e.reason)
}

// A native Responses function is executed by the caller only after delivery.
// Validate before exposing any output, so an invalid turn can be regenerated
// once without publishing duplicate tool identities or partial arguments.
// Hosted tools and stored continuations can already have side effects upstream;
// they are validated too, but are never replayed by this guard.
func (c *CLIClient) doResponsesWithToolValidation(ctx context.Context, acc *store.Account, path string, payload map[string]any) (*http.Response, error) {
	schemas := map[string]*upstream.Schema{}
	if strings.Trim(path, "/") == "responses" {
		for _, tool := range responses.InterfaceMaps(payload["tools"]) {
			if tool["type"] == "function" {
				if schema := upstream.CompileSchema(tool["parameters"]); schema != nil {
					schemas[responses.StreamString(tool["name"])] = schema
				}
			}
		}
	}
	if len(schemas) == 0 {
		return c.doResponsesUncheckedAt(ctx, acc, path, payload)
	}
	requestPayload := payload
	for attempt := 0; ; attempt++ {
		resp, err := c.doResponsesUncheckedAt(ctx, acc, path, requestPayload)
		if err != nil {
			return nil, err
		}
		err = validateToolResponse(resp, schemas)
		if err == nil {
			return resp, nil
		}
		var invalid *toolArgumentsError
		if !errors.As(err, &invalid) || attempt >= 1 || ctx.Err() != nil || upstream.RemainingAttempts(ctx) == 0 || !toolArgumentsReplaySafe(payload) {
			return nil, err
		}
		requestPayload, err = cloneBuildPayload(payload)
		if err != nil {
			return nil, err
		}
		instructions, _ := requestPayload["instructions"].(string)
		detail := invalid.reason.Error()
		if len(detail) > 512 {
			detail = detail[:512]
		}
		feedback := fmt.Sprintf("The previous response was withheld because the arguments for function %q did not match its declared parameter schema (%s). Regenerate the response, including every required property in each function call and using the declared property types. Do not invent missing information; ask the user if necessary.", invalid.name, detail)
		requestPayload["instructions"] = strings.TrimSpace(instructions + "\n\n" + feedback)
		slog.Warn("Regenerating Grok response after invalid tool arguments", "tool", invalid.name, "attempt", attempt+2)
	}
}

func toolArgumentsReplaySafe(payload map[string]any) bool {
	stored, _ := payload["store"].(bool)
	background, _ := payload["background"].(bool)
	previous, _ := payload["previous_response_id"].(string)
	return !stored && !background && strings.TrimSpace(previous) == "" && !qualityRequestReplayUnsafe(nativeQualityRequest("", payload))
}

// Replay exact upstream bytes after checking their completed function calls.
// Small turns stay in memory; large reasoning/tool output spills to a private
// temporary file which is removed on every success, failure and cancellation.
func validateToolResponse(resp *http.Response, schemas map[string]*upstream.Schema) error {
	buffer := &toolResponseBuffer{}
	defer buffer.Close()
	defer resp.Body.Close()
	var err error
	timing := qualitySignals{FirstVisibleMS: -1}
	started := time.Now()
	validate := func(item map[string]any) error {
		if item["type"] != "function_call" {
			return nil
		}
		name, _ := item["name"].(string)
		schema := schemas[name]
		if schema == nil {
			return nil
		}
		arguments, ok := item["arguments"].(string)
		var object map[string]any
		if !ok || json.Unmarshal([]byte(arguments), &object) != nil || object == nil {
			return &toolArgumentsError{name, errors.New("arguments must be a JSON object")}
		}
		if err := schema.Validate(object); err != nil {
			return &toolArgumentsError{name, err}
		}
		return nil
	}
	validateOutput := func(response map[string]any) error {
		for _, value := range responses.InterfaceMaps(response["output"]) {
			if err := validate(value); err != nil {
				return err
			}
		}
		return nil
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		type pendingCall struct {
			name      string
			arguments strings.Builder
		}
		calls := map[string]*pendingCall{}
		terminal := false
		err = responses.ReadSSEBytes(io.TeeReader(resp.Body, buffer), func(event string, raw []byte) error {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("[DONE]")) {
				return io.EOF
			}
			var message map[string]any
			if err := json.Unmarshal(raw, &message); err != nil {
				return fmt.Errorf("upstream tool response contains invalid JSON: %w", err)
			}
			kind, _ := message["type"].(string)
			if kind == "" {
				kind = event
			}
			switch kind {
			case "response.output_text.delta", "response.refusal.delta":
				if delta, _ := message["delta"].(string); delta != "" {
					timing.observeVisible(time.Since(started).Milliseconds())
				}
			case "response.output_item.added", "response.output_item.done":
				item, _ := message["item"].(map[string]any)
				if item["type"] == "function_call" {
					id, _ := item["id"].(string)
					name, _ := item["name"].(string)
					call := calls[id]
					if call == nil {
						call = &pendingCall{}
						calls[id] = call
					}
					call.name = name
					if arguments, _ := item["arguments"].(string); arguments != "" {
						call.arguments.Reset()
						call.arguments.WriteString(arguments)
					}
					if kind == "response.output_item.done" {
						return validate(item)
					}
				}
			case "response.function_call_arguments.delta":
				id, _ := message["item_id"].(string)
				if call := calls[id]; call != nil {
					delta, _ := message["delta"].(string)
					call.arguments.WriteString(delta)
				}
			case "response.function_call_arguments.done":
				id, _ := message["item_id"].(string)
				if call := calls[id]; call != nil {
					if arguments, ok := message["arguments"].(string); ok {
						call.arguments.Reset()
						call.arguments.WriteString(arguments)
					}
					return validate(map[string]any{"type": "function_call", "name": call.name, "arguments": call.arguments.String()})
				}
			case "response.completed", "response.incomplete":
				response, _ := message["response"].(map[string]any)
				if err := validateOutput(response); err != nil {
					return err
				}
				// Some streams omit done snapshots and only carry argument deltas.
				for _, call := range calls {
					if err := validate(map[string]any{"type": "function_call", "name": call.name, "arguments": call.arguments.String()}); err != nil {
						return err
					}
				}
				terminal = true
				return io.EOF
			case "response.failed", "error":
				terminal = true
				return io.EOF
			}
			return nil
		})
		if errors.Is(err, io.EOF) {
			err = nil
		}
		if err == nil && !terminal {
			err = io.ErrUnexpectedEOF
		}
	} else {
		if _, err = io.Copy(buffer, resp.Body); err == nil {
			var reader io.Reader
			if reader, err = buffer.reader(); err == nil {
				var response map[string]any
				if err = json.NewDecoder(reader).Decode(&response); err == nil {
					err = validateOutput(response)
				}
			}
		}
	}
	if err != nil {
		return fmt.Errorf("upstream tool response validation failed: %w", err)
	}
	reader, err := buffer.reader()
	if err != nil {
		return err
	}
	if buffer.file == nil {
		resp.Body = &validatedToolResponse{ReadCloser: io.NopCloser(reader), timing: timing}
	} else {
		resp.Body = &validatedToolResponse{ReadCloser: &toolResponseFile{File: buffer.file}, timing: timing}
		buffer.file = nil // ownership transfers to resp.Body
	}
	return nil
}

type validatedToolResponse struct {
	io.ReadCloser
	timing qualitySignals
}

// Replaying buffered bytes does not measure generation speed. Retain the real
// upstream text arrival times so validation cannot fabricate a fast output dump
// and incorrectly park a healthy account in either compatibility stream.
func applyToolResponseTiming(body io.Reader, result *qualitySignals) bool {
	buffered, ok := body.(*validatedToolResponse)
	if ok {
		result.FirstVisibleMS = buffered.timing.FirstVisibleMS
		result.LastVisibleMS = buffered.timing.LastVisibleMS
		result.VisibleTimingObserved = buffered.timing.VisibleTimingObserved
	}
	return ok
}

type toolResponseBuffer struct {
	memory bytes.Buffer
	file   *os.File
	size   int
}

func (b *toolResponseBuffer) Write(data []byte) (int, error) {
	if len(data) > toolResponseSizeLimit-b.size {
		return 0, errors.New("upstream tool response exceeds the validation size limit")
	}
	if b.file == nil && b.size+len(data) > toolResponseMemoryLimit {
		file, err := os.CreateTemp("", "orchids-tool-response-*")
		if err != nil {
			return 0, err
		}
		b.file = file
		if _, err = file.Write(b.memory.Bytes()); err != nil {
			return 0, err
		}
		b.memory.Reset()
	}
	var n int
	var err error
	if b.file != nil {
		n, err = b.file.Write(data)
	} else {
		n, err = b.memory.Write(data)
	}
	b.size += n
	return n, err
}

func (b *toolResponseBuffer) reader() (io.Reader, error) {
	if b.file != nil {
		_, err := b.file.Seek(0, io.SeekStart)
		return b.file, err
	}
	return bytes.NewReader(b.memory.Bytes()), nil
}

func (b *toolResponseBuffer) Close() error {
	if b.file != nil {
		return (&toolResponseFile{File: b.file}).Close()
	}
	return nil
}

type toolResponseFile struct{ *os.File }

func (f *toolResponseFile) Close() error {
	err := f.File.Close()
	_ = os.Remove(f.Name())
	return err
}
