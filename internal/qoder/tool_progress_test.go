package qoder

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// The server cannot finish its long tool output until the caller sees progress.
// This distinguishes actual argument streaming from a flush at final completion.
func TestQoderToolProgressBeforeCompletion(t *testing.T) {
	observed := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"write_file","arguments":"{\"content\":\""}}]}}]}`))
		w.(http.Flusher).Flush()
		select {
		case <-observed:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"complete\"}"}}]},"finish_reason":"tool_calls"}]}`)+"event:finish\ndata: {}\n\n")
	}))
	defer server.Close()
	client := NewFromAccount(signedTestAccount(), &config.Config{QoderInferenceURL: server.URL})
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx, stopGeneration := util.WithFirstGenerationTimeout(ctx, time.Second)
	defer stopGeneration()
	var kinds []string
	var arguments strings.Builder
	var completed string
	var completions int
	err := client.SendRequestWithPayload(ctx, upstream.UpstreamRequest{Model: "Qwen3.7-Max", Prompt: "Write a file", StreamToolDeltas: true}, func(e upstream.SSEMessage) {
		kinds = append(kinds, e.Type)
		if e.Type == "model.tool-call" {
			completed, _ = e.Event["input"].(string)
			completions++
		}
		if e.Type == "model.tool-call-delta" {
			part, _ := e.Event["delta"].(string)
			arguments.WriteString(part)
			util.MarkGenerationProgress(ctx)
			once.Do(func() { close(observed) })
		}
	}, nil)
	if err != nil {
		t.Fatalf("tool generation lost before completion: %v; events=%v", err, kinds)
	}
	if arguments.String() != `{"content":"complete"}` || completed != arguments.String() || completions != 1 {
		t.Fatalf("inconsistent tool arguments: streamed=%q completed=%q count=%d", arguments.String(), completed, completions)
	}
	if len(kinds) < 3 || kinds[0] != "model.tool-call-delta" || kinds[len(kinds)-1] != "model.finish" {
		t.Fatalf("missing progress or finish: %v", kinds)
	}
}

func TestQoderStreamedToolValidation(t *testing.T) {
	for _, args := range []string{`{"path":"a.txt"}`, `{"path":"unfinished`} {
		t.Run(args, func(t *testing.T) {
			body := envelope(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"write_file","arguments":`+jsonString(args)+`}}]}}]}`) + envelope(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`) + "event:finish\ndata: {}\n\n"
			var deltas, calls int
			_, err := consumeStreamObserved(strings.NewReader(body), func(e upstream.SSEMessage) {
				if e.Type == "model.tool-call-delta" {
					deltas++
				}
				if e.Type == "model.tool-call" {
					calls++
				}
			}, nil, true)
			if deltas != 1 {
				t.Fatalf("missing argument progress: %d", deltas)
			}
			if args == `{"path":"a.txt"}` {
				if err != nil || calls != 1 {
					t.Fatalf("valid tool failed: calls=%d err=%v", calls, err)
				}
			} else if err == nil || calls != 0 {
				t.Fatalf("incomplete arguments were completed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestQoderCommandNormalizationStaysBuffered(t *testing.T) {
	body := envelope(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"functions.exec_command","arguments":"{\"cmd\":\"pwd\",\"justification\":\"inspect\"}"}}]},"finish_reason":"tool_calls"}]}`) + "event:finish\ndata: {}\n\n"
	var kinds []string
	var arguments string
	_, err := consumeStreamObserved(strings.NewReader(body), func(e upstream.SSEMessage) {
		kinds = append(kinds, e.Type)
		if e.Type == "model.tool-call" {
			arguments, _ = e.Event["input"].(string)
		}
	}, nil, true)
	if err != nil || len(kinds) != 1 || kinds[0] != "model.tool-call" || arguments != `{"cmd":"pwd"}` {
		t.Fatalf("command normalization changed: kinds=%v args=%q err=%v", kinds, arguments, err)
	}
}
