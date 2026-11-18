package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/middleware"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

type explicitResumeRecordingClient struct {
	calls  int
	resume bool
	owner  string
}

func (c *explicitResumeRecordingClient) SendRequestWithPayload(ctx context.Context, _ upstream.UpstreamRequest, emit func(upstream.SSEMessage), _ *debug.Logger) error {
	c.calls++
	c.resume = upstream.ExplicitResume(ctx)
	c.owner = middleware.APIKeyFingerprint(ctx)
	emit(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": "answer"}})
	emit(upstream.SSEMessage{Type: "model.finish", Event: map[string]any{"finishReason": "stop"}})
	return nil
}

func resumeTestAuth(next http.HandlerFunc) http.HandlerFunc {
	return middleware.APIKeyAuth(func(_ context.Context, key string) (*middleware.APIKeyPrincipal, error) {
		if key == "test-managed-key" {
			return &middleware.APIKeyPrincipal{ID: 1}, nil
		}
		return nil, nil
	}, next)
}

func TestExplicitResumeHeaderReachesAuthenticatedUpstream(t *testing.T) {
	for _, entry := range []string{"messages", "chat"} {
		for _, tc := range []struct {
			name   string
			header []string
			want   bool
		}{
			{name: "absent"},
			{name: "true", header: []string{"true"}, want: true},
			{name: "trimmed case-insensitive true", header: []string{" \tTrUe\t "}, want: true},
			{name: "false", header: []string{"false"}},
			{name: "trimmed case-insensitive false", header: []string{" FALSE "}},
		} {
			t.Run(entry+"/"+tc.name, func(t *testing.T) {
				h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
				client := &explicitResumeRecordingClient{}
				h.client = client
				body := `{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`
				path := "/warp/v1/messages"
				handle := h.HandleMessages
				if entry == "chat" {
					path = "/warp/v1/chat/completions"
					handle = func(w http.ResponseWriter, r *http.Request) {
						h.HandleChatRequest(w, r, chatwire.Request{Model: "model-a", Messages: []chatwire.Message{{Role: "user", Content: "hi"}}}, []byte(body))
					}
				}
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer test-managed-key")
				if tc.header != nil {
					r.Header["X-Warp-Resume"] = tc.header
				}
				// HTTP intent is derived anew, not inherited from an earlier
				// request that happened to opt into continuation.
				r = r.WithContext(upstream.WithExplicitResume(r.Context(), true))
				w := httptest.NewRecorder()
				resumeTestAuth(handle)(w, r)
				testutil.Falsef(t, w.Code != http.StatusOK, "status=%d body=%s", w.Code, w.Body.String())
				testutil.Equal(t, client.calls, 1)
				testutil.Equal(t, client.resume, tc.want)
				testutil.NotEqual(t, client.owner, "")
			})
		}
	}
}

func TestExplicitResumeHeaderRejectedBeforeAccountSelection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		headers http.Header
	}{
		{"empty", "/warp/v1/messages", http.Header{"X-Warp-Resume": {""}}},
		{"non-boolean", "/warp/v1/messages", http.Header{"X-Warp-Resume": {"yes"}}},
		{"numeric", "/warp/v1/messages", http.Header{"X-Warp-Resume": {"1"}}},
		{"comma-list", "/warp/v1/messages", http.Header{"X-Warp-Resume": {"true,false"}}},
		{"repeated-conflict", "/warp/v1/messages", http.Header{"X-Warp-Resume": {"true", "false"}}},
		{"repeated-true", "/warp/v1/messages", http.Header{"X-Warp-Resume": {"true", "true"}}},
		{"mixed-case-duplicate", "/warp/v1/messages", http.Header{"X-Warp-Resume": {"true"}, "x-warp-resume": {"false"}}},
		{"nil-values", "/warp/v1/messages", http.Header{"X-Warp-Resume": nil}},
		{"empty-mixed-case-duplicate", "/warp/v1/messages", http.Header{"X-Warp-Resume": {"true"}, "x-warp-resume": nil}},
		{"unsupported-true", "/workbuddy/v1/messages", http.Header{"X-Warp-Resume": {"true"}}},
		{"unsupported-false", "/cline/v1/chat/completions", http.Header{"X-Warp-Resume": {"false"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No client or account pool is configured: selection would return a
			// pool error instead of the required header validation error.
			h := NewWithLoadBalancer(&config.Config{}, nil)
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`))
			r.Header = tc.headers.Clone()
			r.Header.Set("Authorization", "Bearer test-managed-key")
			w := httptest.NewRecorder()
			resumeTestAuth(h.HandleMessages)(w, r)
			testutil.Equal(t, w.Code, http.StatusBadRequest)
			testutil.CheckContain(t, w.Body.String(), "X-Warp-Resume")
		})
	}
}

func TestExplicitResumeHeaderDoesNotGrantModelPermission(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{}, nil)
	client := &explicitResumeRecordingClient{}
	h.client = client
	r := httptest.NewRequest(http.MethodPost, "/warp/v1/messages", strings.NewReader(`{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer test-managed-key")
	r.Header.Set("X-Warp-Resume", "true")
	w := httptest.NewRecorder()
	middleware.APIKeyAuth(func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
		return &middleware.APIKeyPrincipal{ID: 1, AllowedModels: []string{"other-model"}}, nil
	}, h.HandleMessages)(w, r)
	testutil.Equal(t, w.Code, http.StatusForbidden)
	testutil.Equal(t, client.calls, 0)
}

func TestExplicitResumeHeaderDoesNotAuthenticate(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{}, nil)
	client := &explicitResumeRecordingClient{}
	h.client = client
	for _, auth := range []string{"", "Bearer invalid-key"} {
		r := httptest.NewRequest(http.MethodPost, "/warp/v1/messages", strings.NewReader(`{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`))
		r.Header.Set("X-Warp-Resume", "true")
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		resumeTestAuth(h.HandleMessages)(w, r)
		testutil.Equal(t, w.Code, http.StatusUnauthorized)
		testutil.Equal(t, client.calls, 0)
	}
}
