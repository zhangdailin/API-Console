package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"orchids-api/internal/config"
)

func TestGenerationControlsReachUpstream(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		limit            int
		stop             string
	}{
		{"anthropic", "/qoder/v1/messages", `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":12,"temperature":0,"top_p":0.5,"stop_sequences":["END"]}`, 12, "END"},
		{"chat-string", "/qoder/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":12,"max_completion_tokens":7,"temperature":0,"top_p":0.5,"stop":"STOP"}`, 7, "STOP"},
		{"chat-array", "/qoder/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":0,"temperature":0,"top_p":0.5,"stop":["END"]}`, 0, "END"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
			client := &relayRecordingClient{}
			h.client = client
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			h.HandleMessages(rec, req)
			if rec.Code != 200 || len(client.requests) != 1 {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(client.requests), rec.Body.String())
			}
			got := client.requests[0]
			if got.MaxTokens == nil || *got.MaxTokens != tc.limit || got.Temperature == nil || *got.Temperature != 0 || got.TopP == nil || *got.TopP != 0.5 {
				t.Fatalf("controls lost: %#v", got)
			}
			if tc.stop != "" && (len(got.Stop) != 1 || got.Stop[0] != tc.stop) {
				t.Fatalf("stop=%v", got.Stop)
			}
		})
	}
}

func TestGenerationControlsOmittedAndInvalid(t *testing.T) {
	var req ClaudeRequest
	if err := json.Unmarshal([]byte(`{"messages":[],"stop":null}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.outputTokenLimit() != nil || req.Temperature != nil || req.TopP != nil || req.stopSequences() != nil {
		t.Fatal("omitted controls acquired defaults")
	}
	for _, raw := range []string{`{"stop":[]}`, `{"stop_sequences":[]}`} {
		var empty ClaudeRequest
		if err := json.Unmarshal([]byte(raw), &empty); err != nil {
			t.Fatal(err)
		}
		if got := empty.stopSequences(); got == nil || len(got) != 0 {
			t.Fatalf("explicit empty stop lost: %#v", got)
		}
	}
	if err := json.Unmarshal([]byte(`{"stop":42}`), &req); err == nil {
		t.Fatal("numeric stop accepted")
	}
}

func TestQoderSharedRefusalPreservesFullHintOnlyForQoder(t *testing.T) {
	for _, retry := range []int{1, 2, 3, 4} {
		if got := sharedRefusalWaitForChannel(30*time.Second, retry, "Qoder"); got != 30*time.Second {
			t.Fatalf("retry %d wait %v", retry, got)
		}
		if got := sharedRefusalWaitForChannel(30*time.Second, retry, "workbuddy"); got != sharedRefusalWait(30*time.Second, retry) {
			t.Fatalf("other channel changed: %v", got)
		}
	}
}

func TestCountTokensIncludesEntireRequest(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{}, nil)
	count := func(body string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		h.HandleCountTokens(rec, httptest.NewRequest(http.MethodPost, "/qoder/v1/messages/count_tokens", strings.NewReader(body)))
		var got struct {
			InputTokens int `json:"input_tokens"`
		}
		if rec.Code != 200 {
			t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.InputTokens
	}
	base := count(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	for _, body := range []string{
		`{"model":"m","system":"long system context that was previously completely ignored","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"older conversation with important details"},{"role":"assistant","content":"previous assistant response"},{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"substantial tool result that needs to be counted"},{"type":"text","text":"hi"}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.test/i.png"}},{"type":"text","text":"hi"}]}]}`,
	} {
		if got := count(body); got <= base {
			t.Fatalf("full request count=%d base=%d body=%s", got, base, body)
		}
	}
}
