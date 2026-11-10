package doubao

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestCookieJarRoundTripsAndFiltersPassportCSRF(t *testing.T) {
	jar := cookieJar{"sid": "one", "passport_csrf_token": "csrf"}
	testutil.Equal(t, jar.header(true), "sid=one")
	restored := decodeCookies(jar.encode())
	testutil.Equal(t, restored["sid"], "one")
	testutil.Equal(t, restored["passport_csrf_token"], "csrf")
}

func TestCookieHeaderFiltersEveryPassportCSRFPrefix(t *testing.T) {
	jar := cookieJar{"sid": "one", "passport_csrf_token": "a", "passport_csrf_token_default": "b"}
	testutil.Equal(t, jar.header(true), "sid=one")
}

// The IM gateway answers application/json with status 712012002 "unsupported
// encoding type"; only application/json; encoding=utf-8 reaches the handler.
func TestConversationCreateUsesIMEncodingContentTypeAndFingerprint(t *testing.T) {
	var gotType, gotFP string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		gotFP = r.URL.Query().Get("fp")
		_, _ = io.WriteString(w, `{"status_code":0,"downlink_body":{"create_conv_downlink_body":{"conversation_info":{"conversation_id":"1234"}}}}`)
	}))
	defer server.Close()
	client := NewFromAccount(&store.Account{ClientCookie: `{"sid":"x"}`}, &config.Config{DoubaoBaseURL: server.URL})
	id, err := client.createConversation(context.Background(), cookieJar{"sid": "x", "s_v_web_id": "fp-1"}, "")
	testutil.NoError(t, err)
	testutil.Equal(t, id, "1234")
	testutil.Equal(t, gotType, "application/json; encoding=utf-8")
	testutil.Equal(t, gotFP, "fp-1")
}

func TestConversationCreateReportsUpstreamRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status_code":712012002,"status_desc":"不支持编码类型"}`)
	}))
	defer server.Close()
	client := NewFromAccount(&store.Account{ClientCookie: `{"sid":"x"}`}, &config.Config{DoubaoBaseURL: server.URL})
	_, err := client.createConversation(context.Background(), client.jar(), "")
	testutil.Error(t, err)
	if !strings.Contains(err.Error(), "712012002") || !strings.Contains(err.Error(), "不支持编码类型") {
		t.Fatalf("error=%v", err)
	}
}

func TestModelsCarryTheFourStableAliases(t *testing.T) {
	got := Models()
	testutil.Equal(t, len(got), 4)
	for _, id := range []string{"doubao", "doubao-expert", "doubao-work", "doubao-pro"} {
		model, ok := FindModel(strings.ToUpper(id))
		testutil.True(t, ok, "missing %s", id)
		testutil.False(t, model.ModelItemKey == "", "missing upstream mapping for %s", id)
	}
}

func TestConsumeStreamUsesDeltaAndFallsBackToSnapshot(t *testing.T) {
	deltas := []string{}
	text, err := consumeStream(strings.NewReader("event: CHUNK_DELTA\ndata: {\"text\":\"hello\"}\n\nevent: CHUNK_DELTA\ndata: {\"text\":\" world\"}\n\n"), func(value string) { deltas = append(deltas, value) })
	testutil.NoError(t, err)
	testutil.Equal(t, text, "hello world")
	testutil.Equal(t, strings.Join(deltas, ""), "hello world")
	fallback, err := consumeStream(strings.NewReader("event: STREAM_MSG_NOTIFY\ndata: {\"content\":{\"content_block\":[{\"content\":{\"text_block\":{\"text\":\"snapshot\"}}}]}}\n\n"), func(value string) { deltas = append(deltas, value) })
	testutil.NoError(t, err)
	testutil.Equal(t, fallback, "snapshot")
}

func TestConsumeStreamNeverFallsBackToUserEcho(t *testing.T) {
	text, err := consumeStream(strings.NewReader("event: FULL_MSG_NOTIFY\ndata: {\"message\":{\"message_id\":\"user\"}}\n\nevent: STREAM_MSG_NOTIFY\ndata: {\"message\":{\"message_id\":\"user\"},\"content\":{\"content_block\":[{\"content\":{\"text_block\":{\"text\":\"prompt\"}}}]}}\n\n"), func(string) {})
	testutil.NoError(t, err)
	testutil.Equal(t, text, "")
}

func TestToolEnvelopeOnlyEmitsDeclaredObjectArguments(t *testing.T) {
	var messages []upstream.SSEMessage
	emitToolOrText(`{"tool_calls":[{"name":"read","arguments":{"path":"a"}},{"name":"other","arguments":{}}]}`, []any{map[string]any{"type": "function", "function": map[string]any{"name": "read"}}}, func(message upstream.SSEMessage) { messages = append(messages, message) })
	testutil.Equal(t, len(messages), 1)
	testutil.Equal(t, messages[0].Type, "model.tool-call")
	testutil.Equal(t, messages[0].Event["toolName"], "read")
}

func TestAccountInfoAndQRRequestUseConfiguredHosts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/passport/account/info/v2/":
			_, _ = io.WriteString(w, `{"data":{"user_id_str":"42","name":"Doubao"}}`)
		case "/passport/web/get_qrcode/":
			_, _ = io.WriteString(w, `{"data":{"error_code":0,"token":"token","qrcode_index_url":"https://www.doubao.com/flow-account/scan-login?x=1","expire_time_in":120}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewFromAccount(&store.Account{ClientCookie: `{"sid":"x"}`, DoubaoDeviceID: "1000000000000000001", DoubaoWebID: "1000000000000000002", DoubaoTeaUUID: "1000000000000000002"}, &config.Config{DoubaoBaseURL: server.URL, DoubaoAccountsURL: server.URL})
	info, err := client.AccountInfo(context.Background(), client.jar())
	testutil.NoError(t, err)
	testutil.Equal(t, info.UserID, "42")
	qr, err := fetchQR(context.Background(), client, client.jar())
	testutil.NoError(t, err)
	testutil.Equal(t, qr.token, "token")
	if !strings.Contains(qr.target, "scan-login") {
		t.Fatalf("target=%q", qr.target)
	}
}

func TestPassportHeadersAndQRFailuresKeepUsefulSafeDetail(t *testing.T) {
	client := NewFromAccount(nil, &config.Config{})
	headers := client.headers(cookieJar{"passport_csrf_token": "csrf"}, true)
	testutil.Equal(t, headers.Get("Sec-Fetch-Site"), "same-site")
	testutil.Equal(t, headers.Get("X-Tt-Passport-Csrf-Token"), "csrf")
	payload, detail := decodeQRResponse([]byte(`{"data":{"error_code":2035,"description":"invalid request"}}`))
	testutil.Equal(t, payload.ErrorCode, 2035)
	if !strings.Contains(detail, "2035") || !strings.Contains(detail, "invalid request") {
		t.Fatalf("detail=%q", detail)
	}
}

func TestFetchQRReportsBothUpstreamFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"data":{"error_code":2035,"description":"invalid request"}}`)
	}))
	defer server.Close()
	client := NewFromAccount(nil, &config.Config{DoubaoBaseURL: server.URL, DoubaoAccountsURL: server.URL})
	_, err := fetchQR(context.Background(), client, cookieJar{})
	testutil.Error(t, err)
	if !strings.Contains(err.Error(), "http=403") || !strings.Contains(err.Error(), "2035") {
		t.Fatalf("error=%v", err)
	}
}

func TestFetchQRDecodesCompressedPassportJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		_, _ = writer.Write([]byte(`{"data":{"error_code":0,"token":"token","qrcode_index_url":"https://www.doubao.com/flow-account/scan-login?x=1","expire_time_in":120}}`))
		_ = writer.Close()
	}))
	defer server.Close()
	client := NewFromAccount(nil, &config.Config{DoubaoBaseURL: server.URL, DoubaoAccountsURL: server.URL})
	qr, err := fetchQR(context.Background(), client, cookieJar{})
	testutil.NoError(t, err)
	testutil.Equal(t, qr.token, "token")
}

func TestChatBodyUsesConfiguredModel(t *testing.T) {
	raw, err := chatBody("conversation", "hello", "doubao-pro")
	testutil.NoError(t, err)
	if !bytes.Contains(raw, []byte(`"model_item_key":"5"`)) || !bytes.Contains(raw, []byte(`"mode_id":"3"`)) {
		t.Fatalf("body missing pro route: %s", raw)
	}
}
