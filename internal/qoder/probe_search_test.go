package qoder

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"orchids-api/internal/testutil"
)

func TestProbeSearchVariantsPassTheDeclarationThrough(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoded, _ := io.ReadAll(r.Body)
		raw, err := decodeBody(encoded)
		testutil.NoError(t, err)
		var body map[string]any
		testutil.NoError(t, json.Unmarshal(raw, &body))
		key := "none"
		if tools, _ := body["tools"].([]any); len(tools) > 0 {
			key = tools[0].(map[string]any)["type"].(string)
		} else if parameters, _ := body["parameters"].(map[string]any); parameters["enable_search"] == true {
			key = "enable_search"
		}
		mu.Lock()
		bodies[key] = body
		mu.Unlock()
		if key == "enable_search" {
			// A searched answer: text plus a citation block in the chunk.
			_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"content":"Go 1.99 was released on 2026-08-12, see https://go.dev/doc/devel/release"},"finish_reason":"stop"}],"citations":[{"url":"https://go.dev"}]}`)+"event:finish\ndata: {}\n\n")
			return
		}
		if key == "web_search_20250305" {
			_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"go release\"}"}}]},"finish_reason":"tool_calls"}]}`)+"event:finish\ndata: {}\n\n")
			return
		}
		_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"content":"I cannot browse."},"finish_reason":"stop"}]}`)+"event:finish\ndata: {}\n\n")
	}))
	defer server.Close()

	client := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)
	results, err := client.ProbeRounds(context.Background(), "Qwen3.7-Max", ProbeRoundOptions{Rounds: 1, Search: true})
	testutil.NoError(t, err)
	testutil.Equal(t, len(results), 4)

	byName := map[string]ProbeResult{}
	for _, r := range results {
		byName[r.Variant[:2]] = r
	}
	// Each declaration reached the gateway exactly as a client would send it.
	testutil.Equal(t, len(bodies), 4)
	anthropic := bodies["web_search_20250305"]["tools"].([]any)[0].(map[string]any)
	testutil.Equal(t, anthropic["name"], any("web_search"))
	testutil.Equal(t, anthropic["max_uses"], any(float64(5)))
	testutil.MustContain(t, bodies["none"]["chat_context"].(map[string]any)["text"].(string), "Search the web")

	testutil.Equal(t, byName["S0"].Answer, "I cannot browse.")
	testutil.Equal(t, strings.Join(byName["S1"].ToolCalls, ","), "web_search")
	testutil.MustContain(t, byName["S3"].Answer, "go.dev")
	testutil.MustContain(t, strings.Join(byName["S3"].SearchMarkers, " "), "citation=")
	for _, r := range results {
		testutil.Equal(t, r.Outcome, "served")
	}
}
