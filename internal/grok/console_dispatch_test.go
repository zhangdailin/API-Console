package grok

import (
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/grok/console"
	"orchids-api/internal/grok/web"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestConsoleAccountsStayOutOfTheBuildAndWebPlanes(t *testing.T) {
	acc := &store.Account{ID: 5, AccountType: "grok", GrokProvider: console.ProviderConsole, CredentialType: "sso", ClientCookie: "cookie-value-123",
		Enabled: true, GrokModels: []string{"grok-4.5"}, GrokModelsSyncedAt: time.Now()}
	testutil.False(t, web.IsWebAccount(acc))
	testutil.False(t, web.LooksLikeWebAccount(acc))
	testutil.Equal(t, web.ProviderForAccount(acc), console.ProviderConsole)
	// Even with a stray Build-shaped catalog a Console credential never serves Build.
	testutil.False(t, AccountSupportsModel(acc, "grok-4.5"))
	testutil.False(t, NormalizeProvider(acc))
	testutil.Equal(t, acc.GrokProvider, console.ProviderConsole)
	testutil.False(t, catalogEligible(acc))

	admin := adminPort()
	fresh := &store.Account{AccountType: "grok", GrokProvider: console.ProviderConsole, ClientCookie: "sso=cookie-value-123; other=x"}
	testutil.NoError(t, admin.PrepareCreate(fresh))
	testutil.Equal(t, fresh.GrokProvider, console.ProviderConsole)
	testutil.Equal(t, fresh.ClientCookie, "cookie-value-123")
	// The same cookie as a Web account is a different credential key.
	webTwin := &store.Account{AccountType: "grok", GrokProvider: web.ProviderWeb, CredentialType: "sso", ClientCookie: "cookie-value-123"}
	testutil.True(t, admin.CredentialKey(fresh) != admin.CredentialKey(webTwin))
}

func TestConsoleModelsDispatchToTheConsolePlane(t *testing.T) {
	h := NewHandler(nil, nil)
	for _, tc := range []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		body string
		want string
	}{
		{"chat", h.HandleChatCompletions, `{"model":"console/grok-4.5","messages":[{"role":"user","content":"hi"}]}`, "model_not_found"},
		{"responses", h.HandleResponses, `{"model":"console/grok-4.5","input":"hi"}`, "model_not_found"},
		{"messages", h.HandleMessages, `{"model":"console/grok-4.5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, `"type":"error"`},
		{"compact", h.HandleResponsesCompact, `{"model":"console/grok-4.5","input":"hi"}`, "stateless"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.call(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/x", strings.NewReader(tc.body)))
			testutil.MustContain(t, rec.Body.String(), tc.want)
		})
	}
}

func TestMultipartImageEditsDispatchOnTheFormModel(t *testing.T) {
	h := NewHandler(nil, nil)
	var body strings.Builder
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "console/grok-imagine-image")
	_ = writer.WriteField("prompt", "x")
	_ = writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/images/edits", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	h.HandleImagesEdits(rec, req)
	// The Console plane answered: it resolves the console/ route itself.
	testutil.Equal(t, rec.Code, http.StatusNotFound)
	testutil.MustContain(t, rec.Body.String(), "console/grok-imagine-image")
}
