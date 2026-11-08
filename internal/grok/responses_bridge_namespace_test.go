package grok

import (
	"net/http"
	"net/http/httptest"

	"orchids-api/internal/responses"
	"strings"

	"testing"

	"orchids-api/internal/testutil"
)

// A tool the chat layer genuinely cannot serve is still reported, with the same
// message as before: silently dropping it would answer a caller who asked for a
// code interpreter with a model that has none.
func TestResponsesBridgeStillRejectsUnservableToolTypes(t *testing.T) {
	bridge := responses.ResponsesBridgeHandler(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the inner chat handler must not run")
	}, responses.BridgeOptions{})

	for name, tool := range map[string]string{
		"mcp":              `{"type":"mcp","server_label":"docs"}`,
		"code_interpreter": `{"type":"code_interpreter","container":{"type":"auto"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"model":"gpt-5.6-luna","input":"hi","tools":[` + tool + `]}`
			rec := httptest.NewRecorder()
			bridge(rec, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses", strings.NewReader(body)))
			testutil.Equal(t, rec.Code, http.StatusBadRequest)
			testutil.MustContain(t, rec.Body.String(), "requires a native Responses provider")
		})
	}
}

// Codex's custom, apply_patch and tool_search are lowered for these channels
// (internal/responses/codex_bridge_test.go); what cannot be emulated, a
// client-run local_shell or an empty namespace, still stops before upstream.
func TestResponsesBridgeRejectsClientToolFormatsBeforeUpstream(t *testing.T) {
	for _, channel := range []string{"workbuddy", "cline"} {
		bridge := responses.ResponsesBridgeHandler(func(w http.ResponseWriter, r *http.Request) { t.Fatal("unsupported tool reached upstream") }, responses.BridgeOptions{})
		for _, kind := range []string{"namespace", "local_shell"} {
			rec := httptest.NewRecorder()
			body := `{"model":"model","input":"hello","tools":[{"type":"` + kind + `","name":"example"}]}`
			bridge(rec, httptest.NewRequest(http.MethodPost, "/"+channel+"/v1/responses", strings.NewReader(body)))
			testutil.Equal(t, rec.Code, http.StatusBadRequest)
		}
	}
}
