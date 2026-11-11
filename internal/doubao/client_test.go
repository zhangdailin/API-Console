package doubao

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/prompt"
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

// encoding/json decodes JSON numbers into float64, and fmt.Sprint renders
// 710022004 as 7.10022004e+08. That silently turned a rate-limit refusal into an
// unclassified error, so the gateway retried a risk-control verdict 21 times.
func TestStreamErrorKeepsTheIntegerErrorCode(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(`{"error_code":710022002,"error_msg":"busy"}`), &payload); err != nil {
		t.Fatal(err)
	}
	err := upstreamStreamError(payload)
	if !strings.HasPrefix(err.Error(), "status=429:") || !strings.Contains(err.Error(), "710022002") {
		t.Fatalf("error=%v", err)
	}
}

// A shark_admin verify challenge cannot be answered by replaying the request,
// so it must not be classified as a plain rate limit that is retried.
func TestStreamErrorSurfacesTheHumanVerificationChallenge(t *testing.T) {
	decision := `{\"code\":\"10000\",\"from\":\"shark_admin\",\"type\":\"verify\",\"subtype\":\"semantic_reasoning\",\"detail\":\"token\"}`
	raw := `{"error_code":710022004,"error_msg":"rate limited","extra":{"ack":"1","decision":"` + decision + `"}}`
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	err := upstreamStreamError(payload)
	if !strings.Contains(err.Error(), "requires human verification") || !strings.Contains(err.Error(), "shark_admin/verify/semantic_reasoning") {
		t.Fatalf("error=%v", err)
	}
	if strings.HasPrefix(err.Error(), "status=429") {
		t.Fatalf("a verification challenge must not look retryable: %v", err)
	}
}

// The root conversation is stable for an account; recreating it every turn
// doubled IM gateway traffic, which is what the risk controller scores.
func TestMainConversationIsReusedPerAccount(t *testing.T) {
	created := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		created++
		_, _ = io.WriteString(w, `{"status_code":0,"downlink_body":{"create_conv_downlink_body":{"conversation_info":{"conversation_id":"root-1"}}}}`)
	}))
	defer server.Close()
	client := NewFromAccount(&store.Account{ID: 9001, ClientCookie: `{"sid":"x"}`}, &config.Config{DoubaoBaseURL: server.URL})
	first, err := client.mainConversation(context.Background(), client.jar())
	testutil.NoError(t, err)
	second, err := client.mainConversation(context.Background(), client.jar())
	testutil.NoError(t, err)
	testutil.Equal(t, first, "root-1")
	testutil.Equal(t, second, "root-1")
	testutil.Equal(t, created, 1)
}

func TestConversationTurnReusesTheMainConversation(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		_, _ = io.WriteString(w, `{"status_code":0,"downlink_body":{"create_conv_downlink_body":{"conversation_info":{"conversation_id":"root-2"}}}}`)
	}))
	defer server.Close()
	client := NewFromAccount(&store.Account{ID: 9002, ClientCookie: `{"sid":"x"}`}, &config.Config{DoubaoBaseURL: server.URL})
	first, err := client.mainConversation(context.Background(), client.jar())
	testutil.NoError(t, err)
	second, err := client.mainConversation(context.Background(), client.jar())
	testutil.NoError(t, err)
	testutil.Equal(t, first, "root-2")
	testutil.Equal(t, second, "root-2")
	if len(bodies) != 1 {
		t.Fatalf("a turn must not create a second conversation, got %d creations", len(bodies))
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

// An image answer carries no text at all: the only content is a creation block
// whose later snapshots repeat the same image. The turn must still produce
// output, once per image.
func TestConsumeStreamEmitsGeneratedImagesOnce(t *testing.T) {
	placeholder := `event: STREAM_CHUNK
data: {"message_id":"bot","patch_op":[{"patch_object":1,"patch_value":{"content_block":[{"block_type":2074,"content":{"creation_block":{"creations":[{"id":"c1","image":{"status":1,"placeholder":{"width":2048,"height":2048}}}]}}}]}}]}

`
	final := `event: STREAM_CHUNK
data: {"message_id":"bot","patch_op":[{"patch_object":1,"patch_value":{"content_block":[{"block_type":2074,"content":{"creation_block":{"creations":[{"id":"c1","image":{"image_thumb":{"url":"https://cdn.example/thumb.png"},"image_ori_raw":{"url":"https://cdn.example/original.png"}}}]}}}]}}]}

`
	repeat := `event: STREAM_MSG_NOTIFY
data: {"message":{"message_id":"bot"},"content":{"content_block":[{"block_type":2074,"content":{"creation_block":{"creations":[{"id":"c1","image":{"image_ori":{"url":"https://cdn.example/original.png"}}}]}}}]}}

`
	var emitted []string
	text, err := consumeStream(strings.NewReader(placeholder+final+repeat), func(value string) { emitted = append(emitted, value) })
	testutil.NoError(t, err)
	if strings.Count(text, "https://cdn.example/original.png") != 1 {
		t.Fatalf("text=%q", text)
	}
	if strings.Contains(text, "thumb.png") {
		t.Fatalf("the original should win over the thumbnail: %q", text)
	}
	if !strings.Contains(strings.Join(emitted, ""), "![generated image 1](https://cdn.example/original.png)") {
		t.Fatalf("emitted=%q", emitted)
	}
}

// A chat answer can open with patch_type 1 fragments and continue with
// CHUNK_DELTA events. Both are incremental, so dropping either truncates the
// answer (observed: "PROBE-9f2c71" arrived as PRO, BE, then -9f2c71).
func TestConsumeStreamJoinsPatchFragmentsAndDeltas(t *testing.T) {
	stream := `event: STREAM_CHUNK
data: {"message_id":"bot","patch_op":[{"patch_object":1,"patch_value":{"content_block":[{"block_type":10000,"content":{"text_block":{"text":"PRO"}},"patch_type":1}]}}]}

event: STREAM_CHUNK
data: {"message_id":"bot","patch_op":[{"patch_object":1,"patch_value":{"content_block":[{"block_type":10000,"content":{"text_block":{"text":"BE"}},"patch_type":1}]}}]}

event: CHUNK_DELTA
data: {"text":"-9f2c71"}

`
	var emitted []string
	text, err := consumeStream(strings.NewReader(stream), func(v string) { emitted = append(emitted, v) })
	testutil.NoError(t, err)
	testutil.Equal(t, text, "PROBE-9f2c71")
	testutil.Equal(t, strings.Join(emitted, ""), "PROBE-9f2c71")
}

// A non-1 patch type carries the whole answer so far and must not be appended.
func TestConsumeStreamTreatsOtherPatchTypesAsSnapshots(t *testing.T) {
	stream := `event: STREAM_MSG_NOTIFY
data: {"content":{"content_block":[{"block_type":10000,"content":{"text_block":{"text":"hello"}},"patch_type":2}]}}

event: STREAM_MSG_NOTIFY
data: {"content":{"content_block":[{"block_type":10000,"content":{"text_block":{"text":"hello world"}},"patch_type":2}]}}

`
	text, err := consumeStream(strings.NewReader(stream), func(string) {})
	testutil.NoError(t, err)
	testutil.Equal(t, text, "hello world")
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

// The builder split must keep every field the web client depends on, including
// the duplicated values that live in both option and ext.
func TestChatBodyKeepsTheWebClientEnvelope(t *testing.T) {
	raw, err := chatBody("conv-1", "hello", "doubao-work")
	testutil.NoError(t, err)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	meta, _ := body["client_meta"].(map[string]any)
	if meta["conversation_id"] != "conv-1" || meta["bot_id"] != botID {
		t.Fatalf("client_meta=%v", meta)
	}
	option, _ := body["option"].(map[string]any)
	for _, key := range []string{"conversation_init_option", "conversation_init_ext", "model_config", "aggregate_params"} {
		if option[key] == nil {
			t.Fatalf("option missing %s: %v", key, option)
		}
	}
	if option["agent_mode"] != float64(1) || option["conversation_mode"] != float64(2) {
		t.Fatalf("work route lost its agent/conversation mode: %v", option)
	}
	ext, _ := body["ext"].(map[string]any)
	if ext["agent_mode"] != "1" || ext["general_task_param"] != `{"server_do_compare":true}` {
		t.Fatalf("ext=%v", ext)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%v", body["messages"])
	}
}

// A chat-only channel must not advertise Messages or Responses: clients use the
// capability list to choose a wire format, and those routes are unregistered.
func TestPublishedModelsAdvertiseChatOnly(t *testing.T) {
	rows := publishedModels([]*store.Account{{
		AccountType: "doubao", ClientCookie: `{"sid":"x"}`, DoubaoVerifiedAt: time.Now(),
	}}, time.Now())
	if len(rows) != len(models) {
		t.Fatalf("rows=%d", len(rows))
	}
	for _, row := range rows {
		if len(row.Capabilities) != 1 || row.Capabilities[0] != store.CapabilityChat {
			t.Fatalf("%s advertises %v", row.ModelID, row.Capabilities)
		}
	}
	stale := publishedModels([]*store.Account{{
		AccountType: "doubao", ClientCookie: `{"sid":"x"}`, DoubaoVerifiedAt: time.Now().Add(-48 * time.Hour),
	}}, time.Now())
	if len(stale) != 0 {
		t.Fatalf("a stale observation published %d routes", len(stale))
	}
}

// The upload signature has to stay byte-for-byte stable: the gateway rejects a
// mismatch with InvalidAuthorization, and the only way to notice is a live call.
func TestSigV4AuthorizationShape(t *testing.T) {
	creds := uploadCredentials{
		AccessKey:    "AKTEST",
		SecretKey:    "secret",
		SessionToken: "token",
	}
	got := sigV4Authorization(creds, "20261111T010203Z", http.MethodGet, "/", "Action=ApplyImageUpload&Version=2018-08-01")
	for _, want := range []string{
		"AWS4-HMAC-SHA256 Credential=AKTEST/20261111/cn-north-1/imagex/aws4_request",
		"SignedHeaders=x-amz-date;x-amz-security-token",
		"Signature=",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("authorization missing %q: %s", want, got)
		}
	}
	if got != sigV4Authorization(creds, "20261111T010203Z", http.MethodGet, "/", "Action=ApplyImageUpload&Version=2018-08-01") {
		t.Fatal("signature is not deterministic")
	}
}

// Signed query pairs must be encoded and sorted, or the signature differs from
// the one the object store recomputes.
func TestCanonicalQueryEncodesAndSorts(t *testing.T) {
	testutil.Equal(t, canonicalQuery("b=2&a=1&a=0"), "a=0&a=1&b=2")
	testutil.Equal(t, canonicalQuery("FileExtension=.txt"), "FileExtension=.txt")
}

// Attachment blocks must precede the text block and reference the uploaded uri.
func TestAttachmentMessagesCarryTheUploadedURI(t *testing.T) {
	messages := attachmentMessages([]storeURI{{URI: "tos-cn-i-x/a.txt", Name: "a.txt", Size: 3}})
	if len(messages) != 1 {
		t.Fatalf("messages=%v", messages)
	}
	raw, _ := json.Marshal(messages[0])
	for _, want := range []string{`"block_type":10052`, `"uri":"tos-cn-i-x/a.txt"`, `"name":"a.txt"`, `"upload_status":1`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("attachment missing %s: %s", want, raw)
		}
	}
}

// Images and files use different attachment shapes; the model ignores an image
// sent with the file shape.
func TestImageAttachmentUsesTheImageShape(t *testing.T) {
	messages := attachmentMessages([]storeURI{{URI: "tos-cn-i-x/a.png", Name: "a.png", Size: 9, Image: true}})
	raw, _ := json.Marshal(messages[0])
	for _, want := range []string{`"type":2`, `"key":"tos-cn-i-x/a.png"`, `"image":{`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("image attachment missing %s: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), `"file":`) {
		t.Fatalf("an image must not use the file shape: %s", raw)
	}
}

func TestAttachmentDataDecodesInlineFiles(t *testing.T) {
	data, name, ok := attachmentData(prompt.ContentBlock{
		Type:     "input_file",
		FileName: "probe.txt",
		FileData: "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte("hello")),
	})
	testutil.True(t, ok)
	testutil.Equal(t, name, "probe.txt")
	testutil.Equal(t, string(data), "hello")

	_, _, remote := attachmentData(prompt.ContentBlock{Type: "input_file", URL: "https://example.com/a.txt"})
	testutil.False(t, remote, "a remote URL must not trigger a server-side fetch")
}
