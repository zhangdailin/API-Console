package doubao

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
)

func TestDoubaoCookieImportVerifiesAndLimitsDomains(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, err := r.Cookie("sessionid")
		if r.URL.Path != "/passport/account/info/v2/" || err != nil || session.Value != "synthetic" {
			t.Error("session not validated correctly")
		}
		if _, err := r.Cookie("foreign"); err == nil {
			t.Error("foreign cookie sent to Doubao")
		}
		w.Write([]byte(`{"data":{"user_id_str":"synthetic-user","name":"Test user"}}`))
	}))
	defer upstream.Close()
	env := provider.Env{Config: &config.Config{DoubaoBaseURL: upstream.URL}}
	acc, err := importBrowserCookie(t.Context(), env, `[
		{"name":"sessionid","value":"synthetic","domain":".doubao.com","path":"/"},
		{"name":"foreign","value":"discard","domain":"example.net","path":"/"}
	]`)
	if err != nil || acc.UserID != "synthetic-user" || acc.DoubaoVerifiedAt.IsZero() {
		t.Fatalf("could not verify a valid cookie import: %v", err)
	}
	if _, err := parseBrowserCookies("sessionid=synthetic\r\nX-Forged: true"); err == nil {
		t.Fatal("accepted line breaks in Cookie header")
	}
}

func TestDoubaoThinkingFramesStaySeparate(t *testing.T) {
	input := "event: STREAM_CHUNK\ndata: {\"content_block\":[{\"block_type\":10040,\"block_id\":\"start\"}]}\n\n" +
		"event: CHUNK_DELTA\ndata: {\"text\":\"plan\"}\n\n" +
		"event: STREAM_CHUNK\ndata: {\"content_block\":[{\"block_type\":10040,\"block_id\":\"start\"},{\"block_type\":10040,\"block_id\":\"end\"}]}\n\n" +
		"event: CHUNK_DELTA\ndata: {\"text\":\"answer\"}\n\n"
	var text, reasoning string
	got, err := consumeChatStream(strings.NewReader(input), func(s string) { text += s }, func(s string) { reasoning += s })
	if err != nil || got != "answer" || text != "answer" || reasoning != "plan" {
		t.Fatalf("incorrect stream text=%q reasoning=%q full=%q err=%v", text, reasoning, got, err)
	}
}
