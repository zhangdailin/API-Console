package workbuddy

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/responses"
	"orchids-api/internal/upstream"
)

const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

func mustBuildMessages(t *testing.T, req upstream.UpstreamRequest) []ChatMessage {
	t.Helper()
	messages, err := buildMessages(req)
	if err != nil {
		t.Fatal(err)
	}
	return messages
}

func TestWorkBuddyForwardsImagesToChatEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		history string
		url     string
	}{
		{"chat", `[{"role":"user","content":[{"type":"text","text":"Before"},{"type":"image_url","image_url":{"url":"https://example.com/picture.png"}},{"type":"text","text":"After"}]}]`, "https://example.com/picture.png"},
		{"messages", `[{"role":"user","content":[{"type":"text","text":"Before"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + tinyPNG + `"}},{"type":"text","text":"After"}]}]`, "data:image/png;base64," + tinyPNG},
		{"responses", `[{"role":"user","content":[{"type":"input_text","text":"Before"},{"type":"input_image","image_url":"https://example.com/picture.png"},{"type":"input_text","text":"After"}]}]`, "https://example.com/picture.png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.history)
			if tc.name == "responses" {
				var input any
				if err := json.Unmarshal(raw, &input); err != nil {
					t.Fatal(err)
				}
				messages, err := responses.InputToMessages(input)
				if err != nil {
					t.Fatal(err)
				}
				raw, err = json.Marshal(messages)
				if err != nil {
					t.Fatal(err)
				}
			}
			var history []prompt.Message
			if err := json.Unmarshal(raw, &history); err != nil {
				t.Fatal(err)
			}
			served := make(chan bool, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { served <- true }()
				if r.URL.Path != "/v2/chat/completions" {
					t.Errorf("unexpected upstream path: %s", r.URL.Path)
				}
				var body struct {
					Messages []struct {
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" {
					t.Errorf("unexpected messages: %+v", body.Messages)
					return
				}
				var got []map[string]any
				want := []map[string]any{{"type": "text", "text": "Before"}, {"type": "image_url", "image_url": map[string]any{"url": tc.url}}, {"type": "text", "text": "After"}}
				if err := json.Unmarshal(body.Messages[1].Content, &got); err != nil || !reflect.DeepEqual(got, want) {
					t.Errorf("incorrect upstream content: %s (%v)", body.Messages[1].Content, err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Seen\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			}))
			defer server.Close()
			client := &Client{httpClient: server.Client(), baseURL: server.URL, creds: Credentials{AccessToken: "fixture", UID: "fixture"}}
			if err := client.runChat(t.Context(), upstream.UpstreamRequest{Model: "vision-fixture", Messages: history}, time.Second, func(upstream.SSEMessage) {}, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case <-served:
			default:
				t.Fatal("the request did not reach the upstream fixture")
			}
		})
	}
}

func TestWorkBuddyRejectsBadImagesBeforeUpstream(t *testing.T) {
	for _, blocks := range [][]prompt.ContentBlock{
		{{Type: "image_url"}},
		{{Type: "image_url", URL: "data:image/jpeg;base64," + tinyPNG}},
		{{Type: "image_url", URL: "data:image/png;base64,invalid!"}},
		{{Type: "image_url", URL: "file:///image.png"}},
		{{Type: "image_url", URL: "data:image/png;base64," + strings.Repeat("A", base64.StdEncoding.EncodedLen(16<<20)+4)}},
	} {
		client := &Client{} // an invalid image must fail before token lookup
		err := client.runChat(t.Context(), upstream.UpstreamRequest{
			Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Blocks: blocks}}},
		}, time.Second, func(upstream.SSEMessage) {}, nil)
		if _, ok := apperrors.LocalRequestMessage(err); !ok {
			t.Fatalf("wanted a local validation error; got %v", err)
		}
	}
}

func TestWorkBuddyImageCapabilitySnapshot(t *testing.T) {
	got := CatalogImageSupport(CatalogSnapshot([]WorkBuddyModel{
		{ID: "vision", SupportsImages: new(true)},
		{ID: "disabled", SupportsImages: new(true), ImagesDisabled: true},
		{ID: "text", SupportsImages: new(false)},
		{ID: "unknown"},
	}))
	want := map[string]bool{"vision": true, "disabled": false, "text": false}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected observed image capabilities: %+v", got)
	}
}
