package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/middleware"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
)

func TestDoubaoFileReferencesAreOwnerScoped(t *testing.T) {
	s := sessionTestStore(t)
	h := NewMediaHandler(s, func() *config.Config { return nil }, nil)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "notes.md")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(part, "# owner-only notes")
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/doubao/v1/files", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req = req.WithContext(middleware.WithAPIKeyFingerprint(req.Context(), "owner-a"))
	result := httptest.NewRecorder()
	h.Files(result, req)
	var uploaded struct{ ID string }
	if result.Code != http.StatusOK || json.Unmarshal(result.Body.Bytes(), &uploaded) != nil || uploaded.ID == "" {
		t.Fatalf("upload status %d: %s", result.Code, result.Body.String())
	}

	client := &Client{store: s}
	var block prompt.ContentBlock
	if err := json.Unmarshal(fmt.Appendf(nil, `{"type":"file","file":{"file_id":%q}}`, uploaded.ID), &block); err != nil {
		t.Fatal(err)
	}
	file, ok, err := client.chatAttachment(req.Context(), block)
	if err != nil || !ok || file.name != "notes.md" || string(file.data) != "# owner-only notes" {
		t.Fatalf("failed to resolve the owner's file: %v", err)
	}
	other := middleware.WithAPIKeyFingerprint(t.Context(), "owner-b")
	if _, _, err := client.chatAttachment(other, block); err == nil {
		t.Fatal("another key resolved the file")
	}
	for _, tc := range []struct {
		method, owner string
		want          int
	}{
		{http.MethodGet, "owner-b", 404}, {http.MethodDelete, "owner-b", 404}, {http.MethodGet, "owner-a", 200},
		{http.MethodDelete, "owner-a", 200}, {http.MethodGet, "owner-a", 404},
	} {
		request := httptest.NewRequest(tc.method, "/doubao/v1/files/"+uploaded.ID, nil)
		request.SetPathValue("file_id", uploaded.ID)
		w := httptest.NewRecorder()
		h.FileResource(w, request.WithContext(middleware.WithAPIKeyFingerprint(request.Context(), tc.owner)))
		if w.Code != tc.want || strings.Contains(w.Body.String(), "owner-only notes") {
			t.Fatalf("%s %s: status %d, want %d, body %s", tc.method, tc.owner, w.Code, tc.want, w.Body.String())
		}
	}
}

func TestDoubaoMediaGenerationUsesDedicatedUpstream(t *testing.T) {
	account := sessionTestAccount()
	cfg, closeUpstream := mediaTestUpstream(t, func(w http.ResponseWriter, r *http.Request, payload map[string]any) {
		messages, _ := payload["messages"].([]any)
		if len(messages) != 1 || mediaNumber(mediaObject(messages[0])["content_type"]) != 2009 {
			t.Errorf("request did not select the image skill")
		}
		body := mediaObject(mediaObject(messages[0])["content"])
		if body["ratio"] != "1:1" || body["text"] != "red triangle" {
			t.Errorf("incorrect image request: %v", body)
		}
		writeMediaFixture(w, map[string]any{"message": map[string]any{"content_type": 2010, "content": `{"data":[{"image_ori":{"url":"https://cdn.example.net/image.png","width":1024,"height":1024}}]}`}})
	})
	defer closeUpstream()
	h := NewMediaHandler(nil, func() *config.Config { return cfg }, func(context.Context, string, []int64) (*store.Account, func(), error) {
		return account, func() {}, nil
	})
	res := httptest.NewRecorder()
	h.Images(res, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"prompt":"red triangle","size":"1024x1024"}`)))
	if res.Code != 200 || !strings.Contains(res.Body.String(), "https://cdn.example.net/image.png") {
		t.Fatalf("image status %d: %s", res.Code, res.Body.String())
	}
}

func TestDoubaoVideoSwitchesOnlyOnDefiniteQuotaDecline(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		accepted              bool
		code, calls, accounts int
	}{
		{"quota decline", false, http.StatusOK, 2, 2},
		{"accepted then failed", true, http.StatusBadGateway, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sessionTestStore(t)
			a, b := sessionTestAccount(), sessionTestAccount()
			b.ClientCookie = `{"sessionid":"second"}`
			b.UserID = "user-b"
			for _, account := range []*store.Account{a, b} {
				if err := s.CreateAccount(t.Context(), account); err != nil {
					t.Fatal(err)
				}
			}
			calls := make(chan string, 8)
			cfg, closeUpstream := mediaTestUpstream(t, func(w http.ResponseWriter, r *http.Request, payload map[string]any) {
				session, _ := r.Cookie("sessionid")
				if session == nil {
					t.Error("missing session")
					return
				}
				calls <- session.Value
				if payload["task_id"] != nil {
					if payload["task_id"] != "accepted-task" || session.Value != "synthetic-old-session" {
						t.Error("poll used the wrong task or account")
					}
					_, _ = io.WriteString(w, `data: {"event_type":2005,"event_data":{"msg":"今日生成次数已达上限"}}`+"\n\n")
					return
				}
				if session.Value == "synthetic-old-session" {
					if tc.accepted {
						writeMediaFixture(w, map[string]any{"fin_reason": map[string]any{"reason": 1, "async_task": map[string]any{"id": "accepted-task"}}})
					} else {
						_, _ = io.WriteString(w, `data: {"event_type":2005,"event_data":{"msg":"今日生成次数已达上限"}}`+"\n\n")
					}
					return
				}
				writeMediaFixture(w, map[string]any{"message": map[string]any{"content_type": 2021, "content": `{"video_url":"https://cdn.example.net/out.mp4"}`}})
			})
			defer closeUpstream()
			var selections []int64
			h := NewMediaHandler(s, func() *config.Config { return cfg }, func(_ context.Context, model string, excluded []int64) (*store.Account, func(), error) {
				selected := a
				if slices.Contains(excluded, a.ID) {
					selected = b
				}
				selections = append(selections, selected.ID)
				return selected, func() {}, nil
			})
			result := httptest.NewRecorder()
			h.Videos(result, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"prompt":"a short animation","ratio":"16:9"}`)))
			if result.Code != tc.code || len(calls) != tc.calls || len(selections) != tc.accounts {
				t.Fatalf("status %d, upstream %d, selections %v: %s", result.Code, len(calls), selections, result.Body.String())
			}
			if tc.accepted && !strings.Contains(result.Body.String(), "upstream_no_result") {
				t.Fatalf("accepted task failure was not reported: %s", result.Body.String())
			}
			account, err := s.GetAccount(t.Context(), a.ID)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if cooled := store.ModelCooldownKind(account, VideoModel, now) != ""; cooled == tc.accepted ||
				!account.Enabled || account.StatusCode != "" || store.ModelCooldownKind(account, ImageModel, now) != "" {
				t.Fatal("video quota modified an unrelated account state or was not recorded")
			}
		})
	}
}

func TestDoubaoImageEditRejectsPrivateReference(t *testing.T) {
	h := NewMediaHandler(nil, func() *config.Config { return nil }, func(context.Context, string, []int64) (*store.Account, func(), error) {
		t.Fatal("the request reached account selection")
		return nil, nil, nil
	})
	result := httptest.NewRecorder()
	h.ImageEdits(result, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"prompt":"change the background","image":"http://127.0.0.1/secret"}`)))
	if result.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status %d: %s", result.Code, result.Body.String())
	}
}

func mediaTestUpstream(t *testing.T, fn func(http.ResponseWriter, *http.Request, map[string]any)) (*config.Config, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/samantha/chat/completion" {
			t.Errorf("unexpected upstream route: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("invalid upstream request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fn(w, r, body)
	}))
	return &config.Config{DoubaoBaseURL: server.URL, DoubaoRequestGapMS: 1}, server.Close
}

func writeMediaFixture(w io.Writer, detail map[string]any) {
	data, _ := json.Marshal(detail)
	event, _ := json.Marshal(map[string]any{"event_type": 2001, "event_data": string(data)})
	fmt.Fprintf(w, "data: %s\n\n", event)
}
