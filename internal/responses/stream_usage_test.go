package responses

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/store"
)

func completedStreamResponse(t *testing.T, body string) map[string]any {
	t.Helper()
	var completed map[string]any
	if err := ConsumeSSE(strings.NewReader(body), func(event SSEEvent) error {
		if event.Event == "response.completed" {
			var payload struct {
				Response map[string]any `json:"response"`
			}
			if err := json.Unmarshal(event.Data(), &payload); err != nil {
				return err
			}
			completed = payload.Response
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if completed == nil {
		t.Fatalf("missing completed response: %s", body)
	}
	return completed
}

func TestChatStreamUsageIsReportedWithoutInventingUnknownCounters(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames string
		want   map[string]any
	}{
		{name: "absent"},
		{name: "null", frames: "data: {\"choices\":[],\"usage\":null}\n\n"},
		{name: "empty", frames: "data: {\"choices\":[],\"usage\":{}}\n\n"},
		{name: "partial", frames: "data: {\"choices\":[],\"usage\":{\"completion_tokens\":5}}\n\n", want: map[string]any{"output_tokens": float64(5)}},
		{name: "known zero", frames: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0}}\n\n", want: map[string]any{"input_tokens": float64(0), "output_tokens": float64(0), "total_tokens": float64(0)}},
		{name: "usage-only terminal chunk", frames: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8,\"prompt_tokens_details\":{\"cached_tokens\":2}}}\n\n", want: map[string]any{"input_tokens": float64(3), "output_tokens": float64(5), "total_tokens": float64(8), "input_tokens_details": map[string]any{"cached_tokens": float64(2)}}},
		{name: "split counters are snapshots not increments", frames: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3}}\n\ndata: {\"choices\":[],\"usage\":{\"completion_tokens\":2}}\n\ndata: {\"choices\":[],\"usage\":{\"completion_tokens\":5}}\n\n", want: map[string]any{"input_tokens": float64(3), "output_tokens": float64(5), "total_tokens": float64(8)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" + tc.frames + "data: [DONE]\n\n"
			w := httptest.NewRecorder()
			var recorded map[string]any
			WriteStreamFromChatReader(w, CreateRequest{Model: "test"}, strings.NewReader(input), StreamOptions{OnComplete: func(response map[string]any) { recorded = response }})
			response := completedStreamResponse(t, w.Body.String())
			got, _ := response["usage"].(map[string]any)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("completed usage=%#v, want %#v", got, tc.want)
			}
			if recorded == nil {
				t.Fatal("terminal response not recorded")
			}
			if tc.want == nil {
				if _, exists := recorded["usage"]; exists {
					t.Fatalf("recorded invented usage: %#v", recorded)
				}
			}
		})
	}
}

func TestTypedBridgeRetainsInternalStreamUsageAcrossChannels(t *testing.T) {
	for _, channel := range []string{"workbuddy", "cline", "warp"} {
		for _, base := range []string{"/" + channel, "/" + channel + "/v1"} {
			t.Run(base, func(t *testing.T) {
				memory := store.NewMemoryResponseStore(0)
				bridge := ResponsesBridgeHandler(func(http.ResponseWriter, *http.Request) { t.Error("HTTP fallback called") }, BridgeOptions{Store: memory, ExecuteChat: func(w http.ResponseWriter, r *http.Request, chat chatwire.Request, raw []byte) {
					if r.URL.Path != base+"/messages" || r.Body != http.NoBody || !chat.Stream || chat.SourceOperation != "responses" || chatwire.SourceOperation(r.Context()) != "responses" {
						t.Errorf("internal request lost format/channel/source: path=%s chat=%#v", r.URL.Path, chat)
					}
					var decoded chatwire.Request
					if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Model != "test" || !decoded.Stream {
						t.Errorf("Chat payload changed: %s, err=%v", raw, err)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"test\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				}})
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodPost, base+"/responses", strings.NewReader(`{"model":"test","input":"hello","stream":true}`))
				bridge(w, r)
				response := completedStreamResponse(t, w.Body.String())
				want := map[string]any{"input_tokens": float64(3), "output_tokens": float64(5), "total_tokens": float64(8)}
				if !reflect.DeepEqual(response["usage"], want) {
					t.Fatalf("completed usage=%#v, want %#v", response["usage"], want)
				}
				stored, err := memory.GetStoredResponse(r.Context(), response["id"].(string), OwnerHash(r.Context()))
				if err != nil {
					t.Fatal(err)
				}
				var saved map[string]any
				if err := json.Unmarshal(stored.Body, &saved); err != nil || !reflect.DeepEqual(saved["usage"], want) {
					t.Fatalf("stored usage differs: %s, err=%v", stored.Body, err)
				}
			})
		}
	}
}
