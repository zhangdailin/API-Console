package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func webFetchGuardSchema() map[string]any {
	return map[string]any{
		"type": "object", "required": []any{"url", "prompt"}, "additionalProperties": false,
		"properties": map[string]any{"url": map[string]any{"type": "string"}, "prompt": map[string]any{"type": "string"}},
	}
}

func webFetchGuardPayload(stream bool) map[string]any {
	return map[string]any{
		"model": "grok-4.6", "input": "Fetch https://example.com and summarize it", "stream": stream, "store": false,
		"instructions": "Original instructions.",
		"tools":        []any{map[string]any{"type": "function", "name": "WebFetch", "parameters": webFetchGuardSchema()}},
	}
}

func writeGuardToolResponse(w http.ResponseWriter, stream, invalid bool) {
	args, id := `{"url":"https://example.com","prompt":"Summarize the page"}`, "valid_call"
	if invalid {
		args, id = `{"url":"https://example.com"}`, "invalid_call"
	}
	item := map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": id, "name": "WebFetch", "arguments": args}
	result := map[string]any{"id": "resp_guard", "status": "completed", "output": []any{item}}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	events := []map[string]any{
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": item["id"], "type": "function_call", "call_id": id, "name": "WebFetch", "arguments": ""}},
		{"type": "response.function_call_arguments.delta", "item_id": item["id"], "delta": args},
		{"type": "response.function_call_arguments.done", "item_id": item["id"], "arguments": args},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": result},
	}
	for _, event := range events {
		raw, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], raw)
	}
}

func guardTestClient(t *testing.T, handler http.HandlerFunc) (*CLIClient, *store.Account) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := NewCLIClient(&config.Config{GrokCLIBaseURL: server.URL})
	c.httpClient, c.oauth.httpClient = server.Client(), server.Client()
	acc := &store.Account{AccountType: "grok", GrokProvider: ProviderBuild, CredentialType: "oauth", Enabled: true,
		OAuthAccessToken: jwtWithClaims(t, `{"sub":"guard-user","team_id":"guard-team"}`), OAuthExpiresAt: time.Now().Add(time.Hour)}
	return c, acc
}

func TestToolArgumentsGuardRegeneratesBeforeDelivery(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls atomic.Int32
			c, acc := guardTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				_ = json.NewDecoder(r.Body).Decode(&payload)
				attempt := calls.Add(1)
				if attempt == 2 && !strings.Contains(payload["instructions"].(string), "every required property") {
					t.Error("regeneration omitted schema feedback")
				}
				writeGuardToolResponse(w, stream, attempt == 1)
			})
			payload := webFetchGuardPayload(stream)
			before, _ := json.Marshal(payload)
			ctx, budget := upstream.WithAttemptBudget(context.Background(), 3)
			resp, err := c.doResponsesAt(ctx, acc, "/responses", payload)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil || strings.Contains(string(raw), "invalid_call") || !strings.Contains(string(raw), "valid_call") || !strings.Contains(string(raw), "Summarize the page") {
				t.Fatalf("invalid turn reached the caller: %s, err=%v", raw, err)
			}
			after, _ := json.Marshal(payload)
			if calls.Load() != 2 || budget.Used() != 2 || !bytes.Equal(before, after) {
				t.Fatalf("calls=%d budget=%d mutated=%v", calls.Load(), budget.Used(), !bytes.Equal(before, after))
			}
		})
	}
}

func TestToolArgumentsGuardStopsAndDoesNotReplayUnsafeTurns(t *testing.T) {
	for _, scenario := range []string{"exhausted", "budget", "hosted", "stored", "continuation", "mcp"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			c, acc := guardTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				writeGuardToolResponse(w, true, true)
			})
			payload := webFetchGuardPayload(true)
			limit, wanted := 4, int32(1)
			switch scenario {
			case "exhausted":
				wanted = 2
			case "budget":
				limit = 1
			case "hosted":
				payload["tools"] = append(payload["tools"].([]any), map[string]any{"type": "web_search"})
			case "stored":
				payload["store"] = true
			case "continuation":
				payload["previous_response_id"] = "resp_previous"
			case "mcp":
				payload["mcp_servers"] = []any{map[string]any{"url": "https://example.com"}}
			}
			ctx, _ := upstream.WithAttemptBudget(context.Background(), limit)
			resp, err := c.doResponsesAt(ctx, acc, "/responses", payload)
			var invalid *toolArgumentsError
			if resp != nil || !errors.As(err, &invalid) || calls.Load() != wanted || shouldSwitchGrokAccount(err) {
				t.Fatalf("resp=%v err=%v calls=%d, wanted %d", resp, err, calls.Load(), wanted)
			}
			rec := httptest.NewRecorder()
			writeGrokUpstreamError(rec, err)
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "invalid_tool_arguments") || strings.Contains(rec.Body.String(), "WebFetch") {
				t.Fatalf("unsafe public error: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestToolArgumentsGuardReplaysExactBytesAndCleansSpool(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	for _, invalid := range []bool{false, true} {
		rec := httptest.NewRecorder()
		writeGuardToolResponse(rec, true, invalid)
		raw := []byte(":" + strings.Repeat("x", toolResponseMemoryLimit) + "\n\n" + rec.Body.String())
		resp := &http.Response{Header: rec.Header(), Body: io.NopCloser(bytes.NewReader(raw))}
		err := validateToolResponse(resp, map[string]*upstream.Schema{"WebFetch": upstream.CompileSchema(webFetchGuardSchema())})
		if invalid {
			var failure *toolArgumentsError
			if !errors.As(err, &failure) {
				t.Fatalf("missing validation failure: %v", err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("spooled response changed: err=%v", err)
			}
			_ = resp.Body.Close()
		}
		files, _ := os.ReadDir(os.TempDir())
		if len(files) != 0 {
			t.Fatalf("temporary response leaked after invalid=%v: %v", invalid, files)
		}
	}
}

func TestToolArgumentsGuardCompatibilityEndpoints(t *testing.T) {
	for _, surface := range []string{"chat", "messages", "responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", surface, stream), func(t *testing.T) {
				var calls atomic.Int32
				c, acc := guardTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					var wire map[string]any
					_ = json.NewDecoder(r.Body).Decode(&wire)
					stream, _ := wire["stream"].(bool)
					writeGuardToolResponse(w, stream, calls.Add(1) == 1)
				})
				h, s, _ := setupValidationHandler(t)
				if err := s.CreateModel(context.Background(), &store.Model{Channel: "Grok", ModelID: "grok-4.6", Name: "grok-4.6", Status: store.ModelStatusAvailable, Verified: true}); err != nil {
					t.Fatal(err)
				}
				acc.GrokModels, acc.GrokModelsSyncedAt = []string{"grok-4.6"}, time.Now()
				if err := s.CreateAccount(context.Background(), acc); err != nil {
					t.Fatal(err)
				}
				c.cfg.GrokLowLatency = true
				h.cfg, h.cliClient = c.cfg, c
				payload := map[string]any{"model": "grok-4.6", "stream": stream, "max_tokens": 1000,
					"messages": []any{map[string]any{"role": "user", "content": "Fetch https://example.com and summarize it"}}}
				switch surface {
				case "chat":
					payload["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "WebFetch", "parameters": webFetchGuardSchema()}}}
				case "messages":
					payload["tools"] = []any{map[string]any{"name": "WebFetch", "input_schema": webFetchGuardSchema()}}
				case "responses":
					payload = webFetchGuardPayload(stream)
				}
				raw, _ := json.Marshal(payload)
				rec := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodPost, "/grok/v1/"+surface, bytes.NewReader(raw))
				switch surface {
				case "chat":
					h.HandleChatCompletions(rec, r)
				case "messages":
					h.HandleMessages(rec, r)
				case "responses":
					h.HandleResponses(rec, r)
				}
				if rec.Code != http.StatusOK || calls.Load() != 2 || strings.Contains(rec.Body.String(), "invalid_call") || !strings.Contains(rec.Body.String(), "Summarize the page") {
					t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
				}
			})
		}
	}
}

func TestToolArgumentsGuardPreservesUpstreamGenerationTiming(t *testing.T) {
	// Replaying a validated stream in microseconds must not overwrite the
	// original generation interval used to identify degraded account output.
	timing := qualitySignals{FirstVisibleMS: 5000, LastVisibleMS: 9000, VisibleTimingObserved: true}
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n"
	for _, native := range []bool{false, true} {
		body := &validatedToolResponse{ReadCloser: io.NopCloser(strings.NewReader(stream)), timing: timing}
		rec := httptest.NewRecorder()
		var result chatOutcome
		if native {
			_, _, result = copyNativeResponseWithQuality(rec, body, "text/event-stream", "grok-4.6", nil, newNativeResponseQuality(rec, qualityHoldPolicy{}))
		} else {
			result = (&Handler{}).streamBuildChatHolding(rec, &chatwire.Request{Model: "grok-4.6"}, body, nil)
		}
		if result.Err != nil || result.Quality.FirstVisibleMS != 5000 || result.Quality.visibleDurationMS() != 4000 {
			t.Fatalf("native=%v result=%+v", native, result)
		}
	}
}

func TestToolArgumentsGuardValidatesDeltasWithoutSnapshots(t *testing.T) {
	for _, arguments := range []string{`{"url":"https://example.com"}`, `{"url":"https://example.com","prompt":"Summarize"}`} {
		var stream strings.Builder
		for _, event := range []map[string]any{
			{"type": "response.output_item.added", "item": map[string]any{"id": "fc_1", "call_id": "call_1", "name": "WebFetch", "type": "function_call", "arguments": ""}},
			{"type": "response.function_call_arguments.delta", "item_id": "fc_1", "delta": arguments},
			{"type": "response.completed", "response": map[string]any{"id": "resp_1", "status": "completed"}},
		} {
			raw, _ := json.Marshal(event)
			fmt.Fprintf(&stream, "data: %s\n\n", raw)
		}
		resp := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream.String()))}
		err := validateToolResponse(resp, map[string]*upstream.Schema{"WebFetch": upstream.CompileSchema(webFetchGuardSchema())})
		missing := !strings.Contains(arguments, "prompt")
		if (err != nil) != missing {
			t.Fatalf("arguments=%s err=%v", arguments, err)
		}
		_ = resp.Body.Close()
	}
}

func TestToolArgumentsGuardCancellationCleansBuffer(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	started := make(chan struct{})
	var calls atomic.Int32
	c, acc := guardTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, ":%s\n\n", strings.Repeat("x", toolResponseMemoryLimit+100))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()
	resp, err := c.doResponsesAt(ctx, acc, "/responses", webFetchGuardPayload(true))
	if resp != nil || err == nil || calls.Load() != 1 {
		t.Fatalf("resp=%v err=%v calls=%d", resp, err, calls.Load())
	}
	files, _ := os.ReadDir(os.TempDir())
	if len(files) != 0 {
		t.Fatalf("cancelled response leaked a buffer: %v", files)
	}
}
