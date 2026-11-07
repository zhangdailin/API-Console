package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

// The Grok tools page once sent reasoning_effort with every video; the
// endpoint refused it as "invalid request" and the page could not say why.
func TestVideosRequestNamesAnUnknownField(t *testing.T) {
	body := `{"model":"grok-imagine-video","prompt":"一只红苹果放在桌上","seconds":6,"size":"1792x1024","resolution_name":"480p","preset":"fun","input_references":[],"reasoning_effort":"xhigh"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/grok/v1/videos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	(&Handler{}).HandleVideosCreate(rec, req)
	testutil.Equal(t, rec.Code, 400)
	var out struct {
		Error struct{ Message string } `json:"error"`
	}
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	testutil.MustContain(t, out.Error.Message, `"reasoning_effort"`)
}

// What the tools page sends now is accepted by the parser.
func TestVideosRequestAcceptsTheToolsPagePayload(t *testing.T) {
	body := `{"model":"grok-imagine-video","prompt":"一只红苹果放在桌上","seconds":6,"size":"1792x1024","resolution_name":"480p","preset":"fun","input_references":[]}`
	req := httptest.NewRequest("POST", "/grok/v1/videos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	parsed, err := parseVideosRequest(req)
	testutil.NoError(t, err)
	testutil.Equal(t, parsed.Seconds, 6)
	testutil.Equal(t, parsed.Preset, "fun")
}
