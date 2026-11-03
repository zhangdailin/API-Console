package grok

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

func TestPeekWebModelRestoresTheBody(t *testing.T) {
	body := `{"model":"grok-chat-fast","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(body))
	isWeb, handled := peekWebModel(httptest.NewRecorder(), req)
	testutil.True(t, isWeb)
	testutil.False(t, handled)
	restored, err := io.ReadAll(req.Body)
	testutil.NoError(t, err)
	testutil.Equal(t, string(restored), body)

	req = httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(`{"model":"grok-4.5"}`))
	isWeb, _ = peekWebModel(httptest.NewRecorder(), req)
	testutil.False(t, isWeb)
}

func TestMessagesRefusesWebModelsWithAPointerToTheServedEndpoints(t *testing.T) {
	h := NewHandler(nil, nil)
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/messages", strings.NewReader(`{"model":"grok-chat-auto","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)))
	testutil.Equal(t, rec.Code, http.StatusBadRequest)
	testutil.MustContain(t, rec.Body.String(), "/chat/completions")
}
