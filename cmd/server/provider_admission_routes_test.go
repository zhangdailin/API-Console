package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/api"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/handler"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
	"orchids-api/internal/upstream"
)

type admissionBlockingClient struct{ entered, release chan struct{} }

func (c *admissionBlockingClient) SendRequestWithPayload(ctx context.Context, _ upstream.UpstreamRequest, emit func(upstream.SSEMessage), _ *debug.Logger) error {
	c.entered <- struct{}{}
	select {
	case <-c.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	emit(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "text-delta", "delta": "done"}})
	emit(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "finish", "finishReason": "stop"}})
	return nil
}

func TestProviderRoutesShareOneGlobalAdmission(t *testing.T) {
	cfg := &config.Config{AdminPass: "secret", AdminToken: "admin", AdminPath: "/admin"}
	_, s, h := newRouteMux(t, "global-admission:", cfg)
	key := "sk-admission"
	digest := sha256.Sum256([]byte(key))
	if err := s.CreateApiKey(context.Background(), &store.ApiKey{Name: "test", KeyHash: hex.EncodeToString(digest[:]), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateModel(context.Background(), &store.Model{Channel: "workbuddy", ModelID: "m", Name: "m", Status: store.ModelStatusAvailable}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAccount(context.Background(), &store.Account{AccountType: "workbuddy", Enabled: true, Weight: 1}); err != nil {
		t.Fatal(err)
	}
	client := &admissionBlockingClient{entered: make(chan struct{}, 1), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(client.release) }) }
	t.Cleanup(release)
	h.SetClientFactory(func(*store.Account, *config.Config) handler.UpstreamClient { return client })
	renderer, err := template.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerRoutes(mux, cfg, s, h, nil, api.New(s, "", cfg.AdminPass, cfg), middleware.NewConcurrencyLimiter(1, 5*time.Second), nil, renderer)
	call := func(path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		mux.ServeHTTP(w, r)
		return w
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("/workbuddy/responses", `{"model":"m","input":"hello"}`) }()
	select {
	case <-client.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first request did not reach upstream")
	}
	for _, path := range []string{"/workbuddy/v1/messages/count_tokens", "/qoder/messages/count_tokens", "/grok/v1/messages/count_tokens"} {
		w := call(path, `{"model":"m"}`)
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), "server_overloaded") {
			t.Fatalf("overload %s = %d %s", path, w.Code, w.Body.String())
		}
	}
	release()
	select {
	case w := <-done:
		if w.Code != http.StatusOK {
			t.Fatalf("first request = %d %s", w.Code, w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first request did not finish")
	}
	if w := call("/workbuddy/v1/messages/count_tokens", `{"model":"m"}`); w.Code != http.StatusOK {
		t.Fatalf("slot was not released: %d %s", w.Code, w.Body.String())
	}
}
