package qoder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/config"
	"orchids-api/internal/httpclient"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/workbuddy"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpstreamReliabilityErrorPolicyUsesActualProviderErrors(t *testing.T) {
	cases := []struct {
		name  string
		acc   *store.Account
		err   error
		want  string
		scope accountpolicy.Scope
	}{
		{"qoder401", &store.Account{AccountType: "qoder"}, classifyStatus(401, "", []byte("{\"message\":\"login expired\"}")), "401", accountpolicy.ScopeCredential},
		{"qoder503", &store.Account{AccountType: "qoder"}, classifyStatus(503, "", []byte("{}")), "", accountpolicy.ScopeNone},
		{"workbuddySession", &store.Account{AccountType: "workbuddy"}, &workbuddy.APIError{HTTPStatus: 200, Code: workbuddy.CodeSessionDead, Message: "Offline user session not found"}, "401", accountpolicy.ScopeCredential},
		{"workbuddyModel", &store.Account{AccountType: "workbuddy"}, &workbuddy.APIError{HTTPStatus: 200, Code: workbuddy.CodeModelThrottle, Message: "model frequency limit"}, "", accountpolicy.ScopeModel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := accountpolicy.Classify(c.acc, c.err, "model-a")
			if v.Status != c.want || v.Scope != c.scope {
				t.Errorf("actual status=%q scope=%s retry=%v switch=%v; want status=%q scope=%s", v.Status, v.Scope, v.Retryable, v.SwitchAccount, c.want, c.scope)
			}
		})
	}
}

func TestStreamStallHonorsIdleBudgetUnderLongParentDeadline(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	c := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(c, s.URL, s.URL, s.URL)
	c.streamIdle = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := c.SendRequestWithPayload(ctx, upstream.UpstreamRequest{Model: "Qwen3.7-Max", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}}, nil, nil)
	if !errors.Is(err, httpclient.ErrStreamIdle("qoder")) {
		t.Fatalf("idle stream error=%v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("local stream timeout canceled the parent")
	}
}
func TestUpstreamReliabilityQoderNestedRetryCount(t *testing.T) {
	old := TransientBackoff
	TransientBackoff = func(int) time.Duration { return 0 }
	defer func() { TransientBackoff = old }()
	var n atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(503)
		io.WriteString(w, "{\"message\":\"provider_error\"}")
	}))
	defer server.Close()
	c := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(c, server.URL, server.URL, server.URL)
	cfg := &config.Config{AdminPass: "test-only-password"}
	config.ApplyDefaults(cfg)
	budgetCtx, _ := upstream.WithAttemptBudget(context.Background(), cfg.MaxRetries+1)
	for i := 0; i <= cfg.MaxRetries; i++ {
		if err := c.SendRequestWithPayload(budgetCtx, upstream.UpstreamRequest{Model: "Qwen3.7-Max", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}}, nil, nil); err == nil {
			t.Fatal("expected failure")
		}
	}
	t.Logf("configured outer retries=%d; actual HTTP attempts across outer budget=%d", cfg.MaxRetries, n.Load())
	if n.Load() > int32(cfg.MaxRetries+1) {
		t.Errorf("nested budget exceeded single request budget: %d HTTP calls vs %d", n.Load(), cfg.MaxRetries+1)
	}
}
