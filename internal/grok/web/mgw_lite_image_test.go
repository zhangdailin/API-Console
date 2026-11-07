package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

// fakeMGW plays grok.com's MGW gateway for one lite drawing: it checks the
// handshake, records session.create and the user turn, and answers with
// progress frames and a finished card_attachment image.
type fakeMGW struct {
	mu      sync.Mutex
	query   string
	cookie  string
	session map[string]any
	prompt  string
	frames  []map[string]any
}

func (f *fakeMGW) serve(t *testing.T) *httptest.Server {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.query, f.cookie = r.URL.RawQuery, r.Header.Get("Cookie")
		f.mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		for {
			var msg map[string]any
			if conn.ReadJSON(&msg) != nil {
				return
			}
			event, _ := msg["event"].(map[string]any)
			switch event["type"] {
			case "session.create":
				f.mu.Lock()
				f.session, _ = event["session"].(map[string]any)
				f.mu.Unlock()
				_ = conn.WriteJSON(map[string]any{"session_id": "s1", "event": map[string]any{"type": "session.created", "client_event_id": event["event_id"]}})
				_ = conn.WriteJSON(map[string]any{"session_id": "s1", "event": map[string]any{"type": "conversation.attached", "conversation": map[string]any{"id": "s1"}}})
			case "conversation.item.create":
				item, _ := event["item"].(map[string]any)
				xGrok, _ := item["x_grok"].(map[string]any)
				chunks, _ := xGrok["input_chunks"].([]any)
				text, _ := chunks[0].(map[string]any)["text"].(map[string]any)
				f.mu.Lock()
				f.prompt, _ = text["text"].(string)
				f.mu.Unlock()
			case "response.create":
				for _, frame := range f.frames {
					_ = conn.WriteJSON(frame)
				}
			}
		}
	}))
}

func imageOutput(url string, progress int, moderated bool) map[string]any {
	data, _ := json.Marshal(map[string]any{"image_chunk": map[string]any{"imageUrl": url, "progress": progress, "moderated": moderated}})
	return map[string]any{"session_id": "s1", "event": map[string]any{"type": "response.grok.output", "output": map[string]any{"card_attachment": map[string]any{"jsonData": string(data)}}}}
}

func done() map[string]any {
	return map[string]any{"session_id": "s1", "event": map[string]any{"type": "response.done", "response": map[string]any{"id": "r1", "status": "completed"}}}
}

func mgwTestClient(server *httptest.Server) *Client {
	cfg := &config.Config{}
	cfg.GrokWebBaseURL = server.URL
	return New(cfg)
}

func TestLiteImageViaMGWDrawsTheFirstFinishedImage(t *testing.T) {
	const first = "https://assets.grok.com/users/u/generated/a/image.jpg"
	fake := &fakeMGW{frames: []map[string]any{
		imageOutput("users/u/generated/a/preview.jpg", 40, false),
		imageOutput("users/u/generated/a/image.jpg", 100, false),
		imageOutput("users/u/generated/b/image.jpg", 100, false),
		done(),
	}}
	server := fake.serve(t)
	defer server.Close()

	result, opened, err := mgwTestClient(server).liteImageViaMGW(context.Background(), "11111111-2222-3333-4444-555555555555", "sso-token", "a red apple")
	testutil.NoError(t, err)
	testutil.True(t, opened, "the turn reached the gateway")
	testutil.Equal(t, result.URL, first)
	testutil.False(t, result.LimitHit, "no limit")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	testutil.Equal(t, fake.prompt, "Drawing: a red apple")
	testutil.MustContain(t, fake.query, "uid=11111111-2222-3333-4444-555555555555")
	testutil.MustContain(t, fake.cookie, "sso=sso-token")
	testutil.MustContain(t, fake.cookie, "x-userid=11111111-2222-3333-4444-555555555555")
	testutil.Equal(t, fake.session["model"], any("fast"))
	xGrok, _ := fake.session["x_grok"].(map[string]any)
	testutil.Equal(t, xGrok["enable_image_generation"], any(true))
	testutil.Equal(t, xGrok["disable_memory"], any(true))
}

func TestLiteImageViaMGWSkipsModeratedImages(t *testing.T) {
	fake := &fakeMGW{frames: []map[string]any{imageOutput("users/u/generated/x/image.jpg", 100, true), done()}}
	server := fake.serve(t)
	defer server.Close()
	result, opened, err := mgwTestClient(server).liteImageViaMGW(context.Background(), "11111111-2222-3333-4444-555555555555", "t", "x")
	testutil.NoError(t, err)
	testutil.True(t, opened, "opened")
	testutil.Equal(t, result.URL, "")
}

func TestLiteImageViaMGWReportsALimit(t *testing.T) {
	fake := &fakeMGW{frames: []map[string]any{{"session_id": "s1", "event": map[string]any{"type": "error", "error": map[string]any{"message": "You have reached your usage limit"}}}}}
	server := fake.serve(t)
	defer server.Close()
	result, opened, err := mgwTestClient(server).liteImageViaMGW(context.Background(), "11111111-2222-3333-4444-555555555555", "t", "x")
	testutil.Error(t, err)
	testutil.True(t, opened, "opened")
	testutil.True(t, result.LimitHit, "a usage limit switches accounts")
}

func TestLiteImageViaMGWNeedsAUserID(t *testing.T) {
	_, opened, err := (&Client{}).liteImageViaMGW(context.Background(), " ", "t", "x")
	testutil.False(t, opened, "nothing was sent")
	testutil.True(t, err == errMGWLiteUnavailable, "the caller falls back to REST")
}

func TestLiteImageViaMGWHandshakeRefusalFallsBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "challenge", http.StatusForbidden)
	}))
	defer server.Close()
	_, opened, err := mgwTestClient(server).liteImageViaMGW(context.Background(), "11111111-2222-3333-4444-555555555555", "t", "x")
	testutil.Error(t, err)
	testutil.False(t, opened, "a refused handshake drew nothing; REST may be tried")
	testutil.False(t, strings.Contains(err.Error(), "t;"), "no cookie in the error")
}

func TestReadMGWLiteImageFallbacks(t *testing.T) {
	frames := func(events ...map[string]any) *strings.Reader {
		var b strings.Builder
		for _, e := range events {
			raw, _ := json.Marshal(map[string]any{"event": e})
			b.Write(raw)
			b.WriteByte('\n')
		}
		return strings.NewReader(b.String())
	}
	// A finished image_chunk in the render placeholder, inside a JSON string.
	nested, _ := json.Marshal(map[string]any{"image_chunk": map[string]any{"imageUrl": "users/u/generated/n/image.jpg", "progress": 100}})
	result, err := readMGWLiteImage(frames(
		map[string]any{"type": "response.chunk", "chunk": map[string]any{"render_generated_image": map[string]any{"data": string(nested)}}},
		map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}},
	))
	testutil.NoError(t, err)
	testutil.Equal(t, result.URL, "https://assets.grok.com/users/u/generated/n/image.jpg")

	// A Markdown image in the assistant text.
	result, err = readMGWLiteImage(frames(
		map[string]any{"type": "response.chunk", "chunk": map[string]any{"text": map[string]any{"text": "Here: ![image](https://assets.grok.com/users/u/generated/m/image.jpg)"}}},
		map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}},
	))
	testutil.NoError(t, err)
	testutil.Equal(t, result.URL, "https://assets.grok.com/users/u/generated/m/image.jpg")

	// Text only: no image, and the log can tell what came back.
	result, err = readMGWLiteImage(frames(
		map[string]any{"type": "response.chunk", "chunk": map[string]any{"text": map[string]any{"text": "I can't draw that."}}},
		map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}},
	))
	testutil.NoError(t, err)
	testutil.Equal(t, result.URL, "")
	testutil.MustContain(t, result.Diagnostics, "response.chunk=1")
	testutil.MustContain(t, result.Diagnostics, "I can't draw that.")
}

func TestReadMGWLiteImageIgnoresPicturesFromTheWeb(t *testing.T) {
	var b strings.Builder
	for _, e := range []map[string]any{
		{"type": "response.chunk", "chunk": map[string]any{"text": map[string]any{"text": "![image](https://upload.example.org/landscape.jpg)"}}},
		{"type": "response.done", "response": map[string]any{"status": "completed"}},
	} {
		raw, _ := json.Marshal(map[string]any{"event": e})
		b.Write(raw)
		b.WriteByte('\n')
	}
	result, err := readMGWLiteImage(strings.NewReader(b.String()))
	testutil.NoError(t, err)
	testutil.Equal(t, result.URL, "")
	testutil.MustContain(t, result.Diagnostics, "landscape.jpg")
}

func TestImageURLShapeElidesIdentifiers(t *testing.T) {
	testutil.Equal(t, imageURLShape("https://assets.grok.com/users/0f8e2c1a-1b2c-4d5e-8f90-123456789abc/generated/a1b2c3d4-5e6f-7a8b-9c0d-112233445566/image.jpg"), "assets.grok.com/users/…/generated/…/image.jpg")
	testutil.Equal(t, imageURLShape(""), "")
}

func mgwFrames(events ...map[string]any) *strings.Reader {
	var b strings.Builder
	for _, e := range events {
		raw, _ := json.Marshal(map[string]any{"event": e})
		b.Write(raw)
		b.WriteByte('\n')
	}
	return strings.NewReader(b.String())
}

func finishedChunk(path string) map[string]any {
	return map[string]any{"image_chunk": map[string]any{"imageUrl": path, "progress": 100}}
}

// The session snapshot carries pictures that are not this drawing (the
// gallery, the account's history); on production they were returned for
// "一只红苹果放在桌上" as a cat, a lake and an autumn path.
func TestReadMGWLiteImageIgnoresTheSessionSnapshot(t *testing.T) {
	result, err := readMGWLiteImage(mgwFrames(
		map[string]any{"type": "conversation.live.snapshot", "items": []any{finishedChunk("users/u/generated/old-cat/image.jpg")}},
		map[string]any{"type": "conversation.live.updated", "items": []any{finishedChunk("users/u/generated/old-lake/image.jpg")}},
		map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}},
	))
	testutil.NoError(t, err)
	testutil.Equal(t, result.URL, "")
}

// The drawing arrives after the text turn ended with a render placeholder.
func TestReadMGWLiteImageWaitsPastResponseDoneForTheDrawing(t *testing.T) {
	result, err := readMGWLiteImage(mgwFrames(
		map[string]any{"type": "response.chunk", "chunk": map[string]any{"render_generated_image": map[string]any{"status": "pending"}}},
		map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}},
		map[string]any{"type": "conversation.live.updated", "items": []any{finishedChunk("users/u/generated/old/image.jpg")}},
		map[string]any{"type": "response.grok.output", "output": map[string]any{"card_attachment": map[string]any{"jsonData": `{"image_chunk":{"imageUrl":"users/u/generated/apple/image.jpg","progress":100}}`}}},
	))
	testutil.NoError(t, err)
	testutil.Equal(t, result.URL, "https://assets.grok.com/users/u/generated/apple/image.jpg")
	testutil.MustContain(t, result.Diagnostics, "chunk.render_generated_image")
}
