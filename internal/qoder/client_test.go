package qoder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

// signedTestAccount builds an account whose credential and derived pair are
// complete, so a request can be assembled without touching the token endpoints.
// observedSnapshot renders the snapshot an upstream refresh records, including
// the wire fields the chat request's model block is rebuilt from.
func observedSnapshot() []string {
	enabled := true
	return CatalogSnapshot(newCatalog([]modelEntry{
		{Key: "qmodel_latest", Name: "Qwen3.7-Max", Format: "openai", Source: "system", Enable: &enabled, MaxInputTokens: 1000000},
		{Key: "qmodel_38max", Name: "Qwen3.8-Max", Format: "openai", Source: "system", Enable: &enabled, IsReasoning: true, MaxInputTokens: 1000000},
		{Key: "dmodel", Name: "DeepSeek-V4-Pro", Format: "openai", Source: "system", Enable: &enabled, IsReasoning: true, MaxInputTokens: 1000000},
	}))
}

func signedTestAccount() *store.Account {
	return &store.Account{
		ID:                1,
		AccountType:       "qoder",
		QoderAccessToken:  "access-1",
		QoderRefreshToken: "refresh-1",
		QoderExpiresAt:    time.Now().Add(6 * time.Hour),
		QoderUserID:       "uid-1",
		QoderMachineID:    "11111111-2222-4333-8444-555555555555",
		QoderRuntimeInfo:  "runtime-info",
		QoderRuntimeKey:   "runtime-key",
		QoderDataPolicy:   true,
		// Routing resolves against the observed catalog, so a client that signs
		// a request needs the snapshot a refresh would have recorded.
		QoderModelIDs: observedSnapshot(),
	}
}

func TestConcurrentRuntimeDerivationIsSingleFlight(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	acc.ID = 0
	acc.QoderRuntimeInfo = ""
	acc.QoderRuntimeKey = ""
	client := NewFromAccount(acc, nil)
	setTestEntropy(client, strings.NewReader(strings.Repeat("runtime-entropy-", 128)))

	const callers = 24
	results := make(chan RuntimeFields, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fields, err := client.ensureRuntimeFields(context.Background(), client.currentCredentials())
			results <- fields
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	want := <-results
	if !want.Complete() {
		t.Fatal("derived runtime fields are incomplete")
	}
	for got := range results {
		if got != want {
			t.Fatal("concurrent callers observed different runtime fields")
		}
	}
}

func TestConcurrentExpiredCredentialRefreshesOnlyOnce(t *testing.T) {
	t.Parallel()

	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "deviceToken/refresh") {
			http.NotFound(w, r)
			return
		}
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_token":"access-new","refresh_token":"refresh-new","expires_in":7200}`))
	}))
	defer server.Close()

	acc := signedTestAccount()
	acc.ID = 0
	acc.QoderAccessToken = "access-old"
	acc.QoderRefreshToken = "refresh-old"
	acc.QoderExpiresAt = time.Now().Add(-time.Minute)
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)

	const callers = 24
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			creds, err := client.ensureAccessToken(context.Background())
			if err == nil && creds.AccessToken != "access-new" {
				err = fmt.Errorf("access token = %q", creds.AccessToken)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestForceRefreshSkipsCredentialAlreadyRotatedByPeer(t *testing.T) {
	t.Parallel()
	acc := signedTestAccount()
	acc.ID = 0
	acc.QoderAccessToken = "access-new"
	acc.QoderRefreshToken = "refresh-new"
	client := NewFromAccount(acc, nil)
	rejected := Credentials{AccessToken: "access-old", RefreshToken: "refresh-old"}
	if err := client.forceRefresh(context.Background(), rejected); err != nil {
		t.Fatalf("peer-rotated credential should be reused: %v", err)
	}
	if got := client.currentCredentials().AccessToken; got != "access-new" {
		t.Fatalf("access token=%q want access-new", got)
	}
}

type failingQoderUpdater struct {
	fail  bool
	calls int
}

func (f *failingQoderUpdater) UpdateAccount(context.Context, *store.Account) error {
	f.calls++
	if f.fail {
		return errors.New("write failed")
	}
	return nil
}

func TestRefreshReportsPersistenceFailureAndRetriesWriteBeforeReuse(t *testing.T) {
	t.Parallel()
	refreshes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes++
		_, _ = w.Write([]byte(`{"device_token":"access-new","refresh_token":"refresh-new","expires_in":7200}`))
	}))
	defer server.Close()

	acc := signedTestAccount()
	acc.QoderAccessToken = "access-old"
	acc.QoderRefreshToken = "refresh-old"
	acc.QoderExpiresAt = time.Now().Add(-time.Minute)
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)
	updater := &failingQoderUpdater{fail: true}
	client.SetAccountStore(updater)
	if _, err := client.ensureAccessToken(context.Background()); err == nil {
		t.Fatal("refresh succeeded even though the rotated token was not persisted")
	}
	updater.fail = false
	creds, err := client.ensureAccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "access-new" {
		t.Fatalf("access token = %q", creds.AccessToken)
	}
	if refreshes != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshes)
	}
	if updater.calls != 2 {
		t.Fatalf("persistence calls = %d, want failed write plus retry", updater.calls)
	}
}

// TestSendRequestSetsTheFullHeaderContract pins the signed header set, including
// the conditional presence rules. The gateway rejects a request that carries an
// empty organization header, so presence is part of correctness, not style.
func TestSendRequestSetsTheFullHeaderContract(t *testing.T) {
	t.Parallel()

	type captured struct {
		headers http.Header
		url     string
		body    []byte
	}
	capturedCh := make(chan captured, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedCh <- captured{headers: r.Header.Clone(), url: r.URL.String(), body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`)))
		_, _ = w.Write([]byte("event:finish\ndata: {}\n\n"))
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "Qwen3.7-Max",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	var got captured
	select {
	case got = <-capturedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the stub server received no request")
	}

	if !strings.Contains(got.url, "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1") {
		t.Fatalf("url = %q, want the fixed chat path and query", got.url)
	}

	// NOTE: net/http canonicalizes header names, so Cosy-ClientType reads back
	// as Cosy-Clienttype and Login-Version as Login-Version.
	// The device id is the identity the credential was authorized under and is
	// sent unchanged; the token and the type are derived from the account, so
	// the same account always presents the same virtual device.
	device := FingerprintFor(acc.QoderMachineID, acc.QoderUserID, acc.QoderAccessToken)
	if device.Token == "" || device.Type == "" {
		t.Fatalf("no device fingerprint was derived for %s", acc.QoderUserID)
	}
	want := map[string]string{
		"Accept":                "text/event-stream",
		"Cache-Control":         "no-cache",
		"Connection":            "keep-alive",
		"Content-Type":          "application/json",
		"Cosy-Business-Product": "ide",
		"Cosy-Business-Type":    "agent",
		"Cosy-Clienttype":       "5",
		"Cosy-Data-Policy":      "agree",
		"Cosy-Machineid":        acc.QoderMachineID,
		"Cosy-Machinetoken":     device.Token,
		"Cosy-Machinetype":      device.Type,
		"Cosy-Scene":            "assistant",
		"Cosy-User":             "uid-1",
		"Login-Version":         "v2",
		"X-Model-Key":           "qmodel_latest",
		"X-Model-Source":        "system",
		"User-Agent":            "Go-http-client/2.0",
	}
	for name, value := range want {
		if got.headers.Get(name) != value {
			t.Errorf("header %s = %q, want %q", name, got.headers.Get(name), value)
		}
	}
	if got.headers.Get("Cosy-Key") == "" || got.headers.Get("Cosy-Key") == "runtime-key" {
		t.Error("Cosy-Key was not rederived using the reference runtime identity")
	}
	if got.headers.Get("Cosy-Date") == "" {
		t.Error("Cosy-Date is empty")
	}
	if auth := got.headers.Get("Authorization"); !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Errorf("Authorization = %q, want a COSY bearer", auth)
	}
	if got.headers.Get("Cosy-Organization-Id") != "" {
		t.Error("an empty organization id was sent as a header")
	}
	if got := len(got.headers); got < 20 {
		t.Errorf("header count = %d, want the full signed set", got)
	}

	// The body is in the private encoding and decodes to the chat payload.
	decoded, err := decodeBodyForTest(got.body)
	if err != nil {
		t.Fatalf("DecodeBody() error = %v", err)
	}
	text := string(decoded)
	for _, want := range []string{`"chat_task":"FREE_INPUT"`, `"session_type":"qoder"`, `"agent_id":"agent_common"`, `"task_id":"common"`, `"stream":true`, `"version":"3"`, `"key":"qmodel_latest"`, `"role":"user"`, `"context_length":1000000`} {
		if !strings.Contains(text, want) {
			t.Errorf("decoded body = %s, want it to contain %s", text, want)
		}
	}

	if len(events) == 0 || events[len(events)-1].Type != "model.finish" {
		t.Fatalf("events = %+v, want a trailing model.finish", events)
	}
	if reason, _ := events[len(events)-1].Event["finishReason"].(string); reason != "end_turn" {
		t.Fatalf("finishReason = %v, want end_turn", events[len(events)-1].Event["finishReason"])
	}
}

// TestSendRequestRefreshesOnceOnUnauthorized proves a single 401 forces one
// token refresh and the retry succeeds. The refresh budget is one attempt: a
// second 401 after a fresh token is a real credential problem, and retrying it
// would hammer the token endpoint.
func TestSendRequestRefreshesOnceOnUnauthorized(t *testing.T) {
	t.Parallel()

	var chatCalls, refreshCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "deviceToken/refresh"):
			refreshCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"device_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`))
		case strings.Contains(r.URL.Path, "agent_chat_generation"):
			chatCalls++
			if chatCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"statusCodeValue":401,"body":"{\"message\":\"login expired\"}"}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)))
			_, _ = w.Write([]byte("event:finish\ndata: {}\n\n"))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)

	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "Qwen3.7-Max",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, nil, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}
	if chatCalls != 2 {
		t.Fatalf("chat calls = %d, want 2 (one rejected, one retried)", chatCalls)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", refreshCalls)
	}
}

func TestForceRefreshRejectsExpiredDurableCredential(t *testing.T) {
	client := NewFromAccount(signedTestAccount(), nil)
	err := client.forceRefresh(context.Background(), Credentials{RefreshToken: "expired", RefreshExpiresAt: time.Now().Add(-time.Minute)})
	if !errors.Is(err, ErrReLoginRequired) {
		t.Fatalf("forceRefresh error=%v want ErrReLoginRequired", err)
	}
	class := apperrors.ClassifyUpstreamError(err.Error())
	if class.Category != "auth" {
		t.Fatalf("class=%+v want auth", class)
	}
}

// TestSendRequestDoesNotReplayAfterOutput proves a retry is refused once content
// has reached the caller: replaying would duplicate the answer.
func TestSendRequestDoesNotReplayAfterOutput(t *testing.T) {
	t.Parallel()

	var chatCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "deviceToken/refresh"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"device_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`))
		case strings.Contains(r.URL.Path, "agent_chat_generation"):
			chatCalls++
			w.Header().Set("Content-Type", "text/event-stream")
			// Emit a delta and then fail without a finish event.
			_, _ = w.Write([]byte(envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"partial"}}]}`)))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "Qwen3.7-Max",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err == nil {
		t.Fatal("SendRequestWithPayload() error = nil for a truncated stream")
	}
	if !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("error = %v, want ErrStreamTruncated", err)
	}
	if chatCalls != 1 {
		t.Fatalf("chat calls = %d, want 1: output must not be replayed", chatCalls)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want only the delivered delta", events)
	}
}

// TestClassifyStatus pins the retry verdicts, including the busy code arriving
// under a 401.
func TestConfiguredClientVersionMatchesReferenceBodyAndHeader(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{QoderClientVersion: "9.8.7"}
	client := NewFromAccount(signedTestAccount(), cfg)
	body, err := buildChatBodyVersion(upstream.UpstreamRequest{}, modelEntry{Key: "m"}, "session", "request", client.clientVersion)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := decodeBodyForTest(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"business":{"product":"ide","version":"1.1.3"`) {
		t.Fatalf("body version is incoherent: %s", raw)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/algo/chat", nil)
	if err := client.applyAuthHeaders(req, credsOf(signedTestAccount()), RuntimeFields{EncryptUserInfo: "info", Key: "key"}, "request", "m", "system", string(body), "/chat"); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Cosy-Version"); got != "9.8.7" {
		t.Fatalf("Cosy-Version=%q", got)
	}
}

func TestReferenceRuntimeIdentityRebuiltAfterTokenRotation(t *testing.T) {
	acc := signedTestAccount()
	acc.ID = 0
	client := NewFromAccount(acc, nil)
	initial := client.currentCredentials()
	before, err := client.ensureRuntimeFields(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	again, err := client.ensureRuntimeFields(context.Background(), initial)
	if err != nil || again != before {
		t.Fatalf("runtime pair changed without a credential rotation: err=%v", err)
	}
	rotated := initial
	rotated.AccessToken = "new-access"
	rotated.RefreshToken = "new-refresh"
	client.storeCredentials(rotated, true, initial.RefreshToken)
	after, err := client.ensureRuntimeFields(context.Background(), rotated)
	if err != nil {
		t.Fatal(err)
	}
	if after == before || !after.Complete() {
		t.Fatal("runtime identity was not renewed when the embedded tokens rotated")
	}
}

func TestReferenceChatBodyCarriesPromptContextAndModel(t *testing.T) {
	model := modelEntry{Key: "qfmodel", DisplayName: "Qwen3.8-Flash", IsReasoning: true, MaxInputTokens: 180000}
	req := upstream.UpstreamRequest{Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "你好 qoder"}}}}
	encoded, err := buildChatBody(req, model, "session-id", "request-id")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := decodeBodyForTest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	context := body["chat_context"].(map[string]interface{})
	if context["text"].(map[string]interface{})["text"] != "你好 qoder" || context["extra"].(map[string]interface{})["originalContent"].(map[string]interface{})["text"] != "你好 qoder" {
		t.Fatalf("chat context did not carry the latest user text: %#v", context)
	}
	contextModel := context["extra"].(map[string]interface{})["modelConfig"].(map[string]interface{})
	if contextModel["key"] != "qfmodel" || contextModel["is_reasoning"] != true {
		t.Fatalf("context model config changed outside the thinking experiment: %#v", contextModel)
	}
	modelConfig := body["model_config"].(map[string]interface{})
	if modelConfig["key"] != "qfmodel" || modelConfig["is_reasoning"] != false || body["business"].(map[string]interface{})["product"] != "ide" {
		t.Fatalf("body model/business mismatch: %#v", body)
	}
	params := body["parameters"].(map[string]interface{})
	if params["max_tokens"] != float64(32768) {
		t.Fatalf("reference max tokens missing: %#v", params)
	}
	for _, field := range []string{"reasoning_effort", "enable_thinking"} {
		if _, present := params[field]; present {
			t.Fatalf("unexpected default %s in %#v", field, params)
		}
	}
}

func TestRefreshedReplayUsesFreshIdentityAndRetryFlag(t *testing.T) {
	original, err := buildChatBodyVersion(upstream.UpstreamRequest{}, modelEntry{Key: "m"}, "session", "old", "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := refreshedReplayBody(original, "new")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := decodeBodyForTest(replayed)
	var body chatBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.RequestID != "new" || body.RequestSetID != "new" || body.ChatRecordID != "new" || body.Business.ID != "new" || !body.IsRetry {
		t.Fatalf("replay identity not refreshed: %+v", body)
	}
}

func TestClassifyStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		status    int
		body      string
		unauth    bool
		retryable bool
		busy      bool
	}{
		{name: "unauthorized", status: 401, body: `{"message":"login expired"}`, unauth: true},
		{name: "forbidden", status: 403, body: `{"message":"nope"}`, unauth: true},
		{name: "busy under 401", status: 401, body: `{"code":"10605","message":"queue"}`, busy: true, retryable: true},
		{name: "server error", status: 503, body: `{}`, retryable: true},
		{name: "rate limited", status: 429, body: `{}`, retryable: true},
		{name: "bad request", status: 400, body: `{"message":"bad"}`},
	}
	for _, tc := range cases {
		err := classifyStatus(tc.status, "", []byte(tc.body))
		var target *attemptStreamError
		if !asAttemptError(err, &target) {
			t.Fatalf("%s: error %v is not an attempt error", tc.name, err)
		}
		if target.unauth != tc.unauth || target.retryable != tc.retryable || target.busy != tc.busy {
			t.Errorf("%s: verdict = unauth=%v retryable=%v busy=%v, want %v/%v/%v",
				tc.name, target.unauth, target.retryable, target.busy, tc.unauth, tc.retryable, tc.busy)
		}
	}
}

func TestRetryAfterDelaySupportsHTTPDateAndCapsSafely(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	if got := retryAfterDelayAt(now.Add(7*time.Second).Format(http.TimeFormat), now); got != 7*time.Second {
		t.Fatalf("HTTP-date Retry-After = %v, want 7s", got)
	}
	if got := retryAfterDelayAt(now.Add(-time.Second).Format(http.TimeFormat), now); got != 0 {
		t.Fatalf("past Retry-After = %v, want 0", got)
	}
	if got := retryAfterDelayAt("9223372036854775807", now); got != 30*time.Second {
		t.Fatalf("huge Retry-After = %v, want cap 30s", got)
	}
}

// TestBusyWaitIsCapped proves a hostile or buggy backoff hint cannot park a
// request indefinitely.
func TestBusyWaitIsCapped(t *testing.T) {
	t.Parallel()

	if got := busyWait("", []byte(`{"retryAfterMs":600000}`)); got != 30*time.Second {
		t.Fatalf("busyWait() = %v, want the 30s cap", got)
	}
	if got := busyWait("", []byte(`{"message":"{\"retryAfterSeconds\":29,\"serviceAvailable\":false}"}`)); got != 29*time.Second {
		t.Fatalf("nested retryAfterSeconds = %v, want 29s", got)
	}
	if got := busyWait("", []byte(`{"queue":{"isQueued":true,"waitTime":1500}}`)); got != 1500*time.Millisecond {
		t.Fatalf("busyWait() = %v, want 1.5s", got)
	}
	if got := busyWait("7", nil); got != 7*time.Second {
		t.Fatalf("busyWait() = %v, want 7s from Retry-After", got)
	}
	if got := busyWait("not-a-number", nil); got != 2*time.Second {
		t.Fatalf("busyWait() = %v, want the 2s default", got)
	}
}

// TestVerifyModelRejectsUnsupportedName proves a bad model name fails before any
// upstream call.
func TestVerifyModelRejectsUnsupportedName(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")
	if err := client.VerifyModel(context.Background(), "definitely-not-a-model"); err == nil {
		t.Fatal("VerifyModel() error = nil for an unsupported model")
	}
}

// TestEnsureRuntimeFieldsRequiresIdentity proves the derivation is refused
// before the UID is known, instead of encrypting an empty identity the gateway
// would reject.
func TestEnsureRuntimeFieldsRequiresIdentity(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	acc.QoderRuntimeInfo = ""
	acc.QoderRuntimeKey = ""
	acc.QoderUserID = ""
	client := NewFromAccount(acc, nil)
	if _, err := client.ensureRuntimeFields(context.Background(), credsOf(acc)); err == nil {
		t.Fatal("ensureRuntimeFields() error = nil without a user id")
	}
}

// TestEnsureRuntimeFieldsDerivesOnce proves the pair is derived on demand and
// then reused, which is what keeps a request from re-deriving the identity
// material on every call.
func TestEnsureRuntimeFieldsDerivesOnce(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	acc.QoderRuntimeInfo = ""
	acc.QoderRuntimeKey = ""
	client := NewFromAccount(acc, nil)
	setTestEntropy(client, strings.NewReader(strings.Repeat("\x11", 4096)))

	first, err := client.ensureRuntimeFields(context.Background(), credsOf(acc))
	if err != nil {
		t.Fatalf("ensureRuntimeFields() error = %v", err)
	}
	if !first.Complete() {
		t.Fatal("the derived pair is incomplete")
	}
	second, err := client.ensureRuntimeFields(context.Background(), credsOf(acc))
	if err != nil {
		t.Fatalf("second ensureRuntimeFields() error = %v", err)
	}
	if first != second {
		t.Fatalf("the pair was re-derived: %+v vs %+v", first, second)
	}
}

// TestApplyAuthHeadersOmitsOrganizationWhenAbsent pins the conditional presence
// rule: the gateway rejects an empty organization header.
func TestApplyAuthHeadersOmitsOrganizationWhenAbsent(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	creds := credsOf(acc)
	fields := RuntimeFields{EncryptUserInfo: "info", Key: "key"}

	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/algo/api/v2/quota/usage?Encode=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.applyAuthHeaders(req, creds, fields, "req-1", "", "", "", signPath(req.URL.String())); err != nil {
		t.Fatalf("applyAuthHeaders() error = %v", err)
	}
	if got := req.Header.Get("Cosy-Organization-Id"); got != "" {
		t.Errorf("Cosy-Organization-Id = %q, want absent", got)
	}
	if got := req.Header.Get("X-Model-Key"); got != "" {
		t.Errorf("X-Model-Key = %q, want absent for a quota read", got)
	}

	creds.OrgID = "org-1"
	creds.OrgTags = []string{"a", "b"}
	req2, err := http.NewRequest(http.MethodPost, "https://example.invalid/algo/api/v2/service/pro/sse/agent_chat_generation", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.applyAuthHeaders(req2, creds, fields, "req-2", "dmodel", "", "body", signPath(req2.URL.String())); err != nil {
		t.Fatalf("applyAuthHeaders() error = %v", err)
	}
	if got := req2.Header.Get("Cosy-Organization-Id"); got != "org-1" {
		t.Errorf("Cosy-Organization-Id = %q, want org-1", got)
	}
	if got := req2.Header.Get("Cosy-Organization-Tags"); got != "a,b" {
		t.Errorf("Cosy-Organization-Tags = %q, want a,b", got)
	}
	if got := req2.Header.Get("X-Model-Source"); got != "" {
		// The source header is gated on the key, not on its own value; an empty
		// source must still be present when a key is sent.
		t.Errorf("X-Model-Source = %q, want present and empty", got)
	}
}

func credsOf(acc *store.Account) Credentials {
	return ResolveCredentials(acc)
}

// TestEntitlementRefusalKeepsTheAccountUsable drives one real streaming request
// against a stub that answers exactly what a live Qoder account without a
// subscription answers, then feeds the resulting error to the shared account
// classifier the handler uses.
//
// This is the whole bug in one assertion. On the live deployment the classifier
// read the upstream "403" out of the message, marked the account status "403",
// and the console showed 「禁止访问」 — and the channel raised a "no usable
// account" alarm — for an account whose credential the gateway had just accepted.
func TestEntitlementRefusalKeepsTheAccountUsable(t *testing.T) {
	t.Parallel()

	// Verbatim from the live gateway: HTTP 200, a business status of 403, and a
	// body naming the pricing page.
	inner := `{"code":"112","message":"{\"pricingUrl\":\"https://qoder.com/pricing?client=qoder\"}"}`
	envelopeBody, err := json.Marshal(map[string]any{
		"headers":         map[string][]string{"Content-Type": {"application/json"}},
		"body":            inner,
		"statusCodeValue": 403,
		"statusCode":      "FORBIDDEN",
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data:" + string(envelopeBody) + "\n\n"))
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)

	requestErr := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "Qwen3.7-Max",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, nil, nil)
	if requestErr == nil {
		t.Fatal("SendRequestWithPayload() error = nil, want an entitlement refusal")
	}
	if !errors.Is(requestErr, ErrNoEntitlement) {
		t.Fatalf("error = %v, want ErrNoEntitlement", requestErr)
	}

	// The handler's own classifier, on the handler's own input.
	if status := apperrors.ClassifyAccountStatus(requestErr.Error()); status != "" {
		t.Fatalf("ClassifyAccountStatus(%q) = %q, want \"\" — a non-empty status disables the account", requestErr.Error(), status)
	}

	// And the reason still reaches whoever reads the request error.
	if !strings.Contains(requestErr.Error(), "pricing") {
		t.Fatalf("error = %v, want the pricing pointer", requestErr)
	}
}

// A subscription refusal is terminal for this account/model pair, but another
// account may have a different plan. Do not retry on the same account or mark
// its credential bad; the handler switches once and records a model cooldown.
func TestEntitlementRefusalSwitchesAccounts(t *testing.T) {
	t.Parallel()

	err := entitlementError(`{"code":"112","message":"{\"pricingUrl\":\"https://qoder.com/pricing?client=qoder\"}"}`)
	class := apperrors.ClassifyUpstreamError(err.Error())
	if class.Category != "model_unavailable" || !class.Retryable || !class.SwitchAccount {
		t.Fatalf("classification = %+v, want switchable model_unavailable", class)
	}
	if status := apperrors.ClassifyAccountStatus(err.Error()); status != "" {
		t.Fatalf("account status = %q, want \"\"", status)
	}
}
