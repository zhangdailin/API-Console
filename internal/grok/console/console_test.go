package console

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/websocket"

	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

const (
	testCookie   = "console-test-sso-cookie-value"
	testAPIKeyID = "0f6c1a5e-7d1b-4f3a-9c2e-1b2a3c4d5e6f"
	testTeamID   = "aa11bb22-cc33-4d44-8e55-ff6677889900"
)

// fakeConsole is a console.x.ai stand-in that checks every DPoP proof.
type fakeConsole struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	tokens   map[string]dpopJWK // access token -> bound key
	models   []string
	mints    atomic.Int32
	lastBody map[string]any
	lastPath string
	refuse   bool
	unlinked bool
	usage    string
}

func b64(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

func newFakeConsole(t *testing.T) *fakeConsole {
	f := &fakeConsole{t: t, tokens: map[string]dpopJWK{}, models: []string{"grok-4.5", "grok-imagine-video", "grok-voice-latest", "grok-stt"},
		usage: `{"weeklyIncludedUsage":{"budgetCents":"500","usedCents":120,"resetsAt":"2099-01-01T00:00:00Z"}}`}
	upgrader := websocket.Upgrader{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/dpop/token":
			f.mint(w, r)
		case r.URL.Path == "/dashboard/api/auth/login":
			if !strings.Contains(r.Header.Get("Cookie"), "sso="+testCookie) {
				t.Errorf("the WorkOS login carried no SSO cookie")
			}
			if f.unlinked {
				http.Redirect(w, r, "https://authenticator.cursor.sh/sign-in", http.StatusFound)
				return
			}
			for name, value := range map[string]string{"WorkosCursorSessionToken": "wt", "WorkosCursorXaiOrigin": "wo", "workos_id": "wid"} {
				http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/dashboard"})
			}
			http.Redirect(w, r, "/dashboard/usage", http.StatusFound)
		case r.URL.Path == "/dashboard/api/dashboard/v2/usage-status":
			cookie := r.Header.Get("Cookie")
			if !strings.Contains(cookie, "WorkosCursorSessionToken=wt") || !strings.Contains(cookie, "workos_id=wid") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, f.usage)
		case r.URL.Path == "/api/auth/session":
			_, _ = io.WriteString(w, `{"session":{"cursorMerged":false}}`)
		case r.URL.Path == "/":
			http.Redirect(w, r, "/team/"+testTeamID, http.StatusTemporaryRedirect)
		case r.URL.Path == "/team/"+testTeamID:
			_, _ = fmt.Fprintf(w, `<script>self.__next_f.push([1,"{\"apiKeyId\":\"%s\",\"disabled\":false}"])</script>`, testAPIKeyID)
		case strings.HasPrefix(r.URL.Path, "/v1/"):
			if !f.authorized(w, r) {
				return
			}
			f.serveV1(w, r, &upgrader)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeConsole) mint(w http.ResponseWriter, r *http.Request) {
	f.mints.Add(1)
	if f.refuse || !strings.Contains(r.Header.Get("Cookie"), "sso="+testCookie) {
		http.Error(w, `{"error":"unauthenticated"}`, http.StatusUnauthorized)
		return
	}
	var body struct {
		JWK dpopJWK `json:"jwk"`
	}
	testutil.CheckNoError(f.t, json.NewDecoder(r.Body).Decode(&body))
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "cnf": map[string]any{"jkt": dpopJWKThumbprint(body.JWK)}})
	token := b64([]byte(`{"alg":"ES256"}`)) + "." + b64(claims) + "." + b64([]byte("sig"))
	f.mu.Lock()
	f.tokens[token] = body.JWK
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "DPoP", "expires_in": 600})
}

// authorized verifies the DPoP proof the way the server would.
func (f *fakeConsole) authorized(w http.ResponseWriter, r *http.Request) bool {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "DPoP ")
	f.mu.Lock()
	jwk, ok := f.tokens[token]
	f.mu.Unlock()
	fail := func(reason string) bool {
		f.t.Errorf("DPoP check failed on %s: %s", r.URL.Path, reason)
		http.Error(w, reason, http.StatusUnauthorized)
		return false
	}
	if !ok {
		return fail("unknown access token")
	}
	if r.Header.Get("x-apikey") != testAPIKeyID {
		return fail("missing x-apikey")
	}
	parts := strings.Split(r.Header.Get("DPoP"), ".")
	if len(parts) != 3 {
		return fail("malformed proof")
	}
	headerRaw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	claimsRaw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	var header struct {
		Typ string  `json:"typ"`
		JWK dpopJWK `json:"jwk"`
	}
	var claims struct {
		HTM string `json:"htm"`
		HTU string `json:"htu"`
		ATH string `json:"ath"`
	}
	_ = json.Unmarshal(headerRaw, &header)
	_ = json.Unmarshal(claimsRaw, &claims)
	if header.Typ != "dpop+jwt" || header.JWK != jwk {
		return fail("proof key is not the bound key")
	}
	x, _ := base64.RawURLEncoding.DecodeString(jwk.X)
	y, _ := base64.RawURLEncoding.DecodeString(jwk.Y)
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return fail("bad proof signature")
	}
	tokenDigest := sha256.Sum256([]byte(token))
	wantHTU := "http://" + r.Host + r.URL.Path
	if claims.HTM != r.Method || claims.HTU != wantHTU || claims.ATH != b64(tokenDigest[:]) {
		return fail(fmt.Sprintf("claims mismatch htm=%s htu=%s", claims.HTM, claims.HTU))
	}
	return true
}

func (f *fakeConsole) serveV1(w http.ResponseWriter, r *http.Request, upgrader *websocket.Upgrader) {
	f.mu.Lock()
	f.lastPath = r.URL.Path
	f.mu.Unlock()
	record := func() {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.lastBody = body
		f.mu.Unlock()
	}
	switch r.URL.Path {
	case "/v1/models":
		data := []map[string]string{}
		for _, id := range f.models {
			data = append(data, map[string]string{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	case "/v1/chat/completions":
		record()
		if stream, _ := f.lastBody["stream"].(bool); !stream {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 1000, "completion_tokens": 500, "total_tokens": 1500, "prompt_tokens_details": map[string]any{"cached_tokens": 200}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\ndata: [DONE]\n\n")
	case "/v1/images/edits":
		record()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"url": "https://imgen.x.ai/a.png"}}})
	case "/v1/responses":
		if r.Header.Get("x-cluster") == "" {
			f.t.Errorf("responses without x-cluster")
		}
		record()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_1", "object": "response", "status": "completed"})
	case "/v1/tts":
		record()
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = io.WriteString(w, "mp3-audio")
	case "/v1/realtime":
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			kind, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			_ = conn.WriteMessage(kind, append([]byte("echo:"), message...))
		}
	default:
		http.NotFound(w, r)
	}
}

func newTestHandler(t *testing.T, f *fakeConsole) (*Handler, *store.Store, *store.Account) {
	t.Helper()
	mini := miniredis.RunT(t)
	st, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "console:"})
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{GrokConsoleBaseURL: f.server.URL, GrokWebBaseURL: f.server.URL}
	h := NewHandler(cfg, loadbalancer.NewWithCacheTTL(st, time.Millisecond))
	h.client.httpClient = f.server.Client()
	acc := &store.Account{Name: "c", AccountType: "grok", GrokProvider: ProviderConsole, ClientCookie: testCookie, Enabled: true}
	testutil.NoError(t, PrepareAccount(acc))
	status, err := verifyWith(context.Background(), h.client, acc)
	testutil.NoError(t, err)
	testutil.Equal(t, status, "")
	testutil.NoError(t, st.CreateAccount(context.Background(), acc))
	testutil.NoError(t, PublishRoutes(context.Background(), st))
	return h, st, acc
}

func TestConsoleVerifyPublishesQualifiedRoutesFromTheCatalogRead(t *testing.T) {
	f := newFakeConsole(t)
	_, st, acc := newTestHandler(t, f)
	ctx := context.Background()
	testutil.Equal(t, len(acc.GrokConsoleModels), 4)
	testutil.Equal(t, len(acc.GrokModels), 0)

	chat, err := st.GetModelByChannelAndModelID(ctx, "grok", "console/grok-4.5")
	testutil.NoError(t, err)
	testutil.Equal(t, chat.Provider, ProviderConsole)
	testutil.Equal(t, chat.UpstreamModel, "grok-4.5")
	testutil.True(t, chat.SupportsCapability(store.CapabilityChat))
	testutil.True(t, chat.SupportsCapability(store.CapabilityMessages))
	voice, err := st.GetModelByChannelAndModelID(ctx, "grok", "console/grok-voice-latest")
	testutil.NoError(t, err)
	testutil.True(t, voice.SupportsCapability(store.CapabilityTTS))
	stt, err := st.GetModelByChannelAndModelID(ctx, "grok", "console/grok-stt")
	testutil.NoError(t, err)
	testutil.True(t, stt.SupportsCapability(store.CapabilitySTT))
	// The bare name stays Build's: no Console row may take it.
	_, err = st.GetModelByChannelAndModelID(ctx, "grok", "grok-4.5")
	testutil.True(t, err != nil)

	// Without an active Console account nothing is published.
	acc.Enabled = false
	testutil.NoError(t, st.UpdateAccount(ctx, acc))
	testutil.NoError(t, PublishRoutes(ctx, st))
	_, err = st.GetModelByChannelAndModelID(ctx, "grok", "console/grok-4.5")
	testutil.True(t, err != nil, "rows are pruned without an observing account")
}

func TestConsoleVerifyReportsARefusedSession(t *testing.T) {
	f := newFakeConsole(t)
	f.refuse = true
	client := NewClient(&config.Config{GrokConsoleBaseURL: f.server.URL})
	client.httpClient = f.server.Client()
	acc := &store.Account{AccountType: "grok", GrokProvider: ProviderConsole, ClientCookie: testCookie}
	status, err := verifyWith(context.Background(), client, acc)
	testutil.Equal(t, status, "401")
	testutil.True(t, err != nil)
}

func TestConsoleChatAndResponsesReachTheUpstreamWithTheUpstreamModel(t *testing.T) {
	f := newFakeConsole(t)
	h, _, _ := newTestHandler(t, f)

	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(`{"model":"console/grok-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.MustContain(t, rec.Body.String(), `"content":"hi"`)
	testutil.Equal(t, f.lastBody["model"], any("grok-4.5"))

	rec = httptest.NewRecorder()
	h.HandleResponses(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/responses", strings.NewReader(`{"model":"console/grok-4.5","input":"hi","store":true}`)))
	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.Equal(t, f.lastBody["store"], any(false))

	rec = httptest.NewRecorder()
	h.HandleResponses(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/responses", strings.NewReader(`{"model":"console/grok-4.5","input":"hi","previous_response_id":"resp_0"}`)))
	testutil.Equal(t, rec.Code, http.StatusBadRequest)

	rec = httptest.NewRecorder()
	h.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(`{"model":"console/grok-unknown","messages":[]}`)))
	testutil.Equal(t, rec.Code, http.StatusNotFound)

	// One DPoP session served every request.
	testutil.Equal(t, f.mints.Load(), int32(1))
}

func TestConsoleAudioSpeechMapsOpenAIFields(t *testing.T) {
	f := newFakeConsole(t)
	h, _, _ := newTestHandler(t, f)
	recorder := &recordingAudit{}
	h.SetAuditLogger(recorder)
	rec := httptest.NewRecorder()
	h.HandleAudioSpeech(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/audio/speech", strings.NewReader(`{"model":"grok-voice-latest","input":"hello","voice":"eve","response_format":"wav"}`)))
	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.Equal(t, rec.Body.String(), "mp3-audio")
	testutil.Equal(t, f.lastBody["text"], any("hello"))
	testutil.Equal(t, f.lastBody["voice_id"], any("eve"))
	testutil.Equal(t, f.lastBody["language"], any("en"))
	format, _ := f.lastBody["output_format"].(map[string]any)
	testutil.Equal(t, format["codec"], any("wav"))
	_, hasModel := f.lastBody["model"]
	testutil.False(t, hasModel)
	// Priced at $15 per 1M characters: "hello" is five.
	testutil.Equal(t, len(recorder.events), 1)
	testutil.Equal(t, recorder.events[0].PricingModel, "grok-voice-tts")
	testutil.Equal(t, recorder.events[0].CostInUSDTicks, int64(5*150_000))

	rec = httptest.NewRecorder()
	h.HandleAudioSpeech(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/audio/speech", strings.NewReader(`{"input":"x","response_format":"midi"}`)))
	testutil.Equal(t, rec.Code, http.StatusBadRequest)
}

func TestConsoleRealtimeRelaysBothDirections(t *testing.T) {
	f := newFakeConsole(t)
	h, _, _ := newTestHandler(t, f)
	gateway := httptest.NewServer(http.HandlerFunc(h.HandleRealtime))
	defer gateway.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/realtime?model=grok-voice-latest", nil)
	testutil.NoError(t, err)
	defer conn.Close()
	testutil.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.update"}`)))
	_, message, err := conn.ReadMessage()
	testutil.NoError(t, err)
	testutil.Equal(t, string(message), `echo:{"type":"session.update"}`)
}

func TestParseHomeSkipsDisabledKeys(t *testing.T) {
	page := []byte(`{\"apiKeyId\":\"11111111-1111-4111-8111-111111111111\",\"disabled\":true},{\"apiKeyId\":\"22222222-2222-4222-8222-222222222222\",\"disabled\":false},\"teamId\":\"` + testTeamID + `\"`)
	home := parseHome(page)
	testutil.Equal(t, home.apiKeyID, "22222222-2222-4222-8222-222222222222")
	testutil.Equal(t, home.teamID, testTeamID)
}

func TestClassifiersKeepConsoleAccountsOutOfTheWebPlane(t *testing.T) {
	acc := &store.Account{AccountType: "grok", GrokProvider: ProviderConsole, CredentialType: "sso", ClientCookie: testCookie}
	testutil.True(t, acc.IsGrokConsole())
	testutil.False(t, acc.IsGrokWeb())
	testutil.True(t, IsConsoleModelID("Console/grok-4.5"))
	testutil.False(t, IsConsoleModelID("grok-4.5"))
	testutil.Equal(t, qualify("grok-stt", ""), "console/grok-stt")
	testutil.True(t, CredentialKey(acc) != "" && !strings.HasPrefix(CredentialKey(acc), "grok:sso:"))
}

type recordingAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recordingAudit) Log(_ context.Context, event audit.Event) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func TestConsoleUsageIsAuditedAndCountedOnTheAccount(t *testing.T) {
	f := newFakeConsole(t)
	h, st, acc := newTestHandler(t, f)
	recorder := &recordingAudit{}
	h.SetAuditLogger(recorder)

	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(`{"model":"console/grok-4.5","messages":[{"role":"user","content":"hi"}]}`)))
	testutil.Equal(t, rec.Code, http.StatusOK)
	rec = httptest.NewRecorder()
	h.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(`{"model":"console/grok-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	testutil.Equal(t, rec.Code, http.StatusOK)

	testutil.Equal(t, len(recorder.events), 2)
	first, second := recorder.events[0], recorder.events[1]
	testutil.Equal(t, first.Provider, ProviderConsole)
	testutil.Equal(t, first.UsageSource, audit.UsageSourceUpstream)
	testutil.Equal(t, first.InputTokens, 1000)
	testutil.Equal(t, first.CachedInputTokens, 200)
	testutil.Equal(t, first.OutputTokens, 500)
	testutil.Equal(t, first.AccountID, acc.ID)
	// Priced from the upstream model's official price.
	testutil.Equal(t, first.PricingModel, "grok-4.5")
	testutil.True(t, first.CostInUSDTicks > 0)
	// The stream's usage comes from its last usage-bearing event.
	testutil.Equal(t, second.InputTokens, 10)
	testutil.Equal(t, second.OutputTokens, 4)

	stored, err := st.GetAccount(context.Background(), acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, stored.UsageTotal, float64(1514))
}

func TestConsoleMultipartImageEditSendsDataURIs(t *testing.T) {
	f := newFakeConsole(t)
	f.models = append(f.models, "grok-imagine-image")
	h, _, _ := newTestHandler(t, f)
	recorder := &recordingAudit{}
	h.SetAuditLogger(recorder)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "console/grok-imagine-image")
	_ = writer.WriteField("prompt", "make it blue")
	part, _ := writer.CreateFormFile("image", "a.png")
	_, _ = part.Write([]byte("\x89PNG\r\n\x1a\n0000000000000000"))
	_ = writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/images/edits", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	h.HandleImagesEdits(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)
	image, _ := f.lastBody["image"].(map[string]any)
	uri, _ := image["url"].(string)
	testutil.True(t, strings.HasPrefix(uri, "data:image/png;base64,"), uri)
	testutil.Equal(t, f.lastBody["model"], any("grok-imagine-image"))
	testutil.Equal(t, len(recorder.events), 1)
	testutil.Equal(t, recorder.events[0].Action, "grok_media_request")
}

func TestConsoleAllowanceIsReadThroughTheWorkOSSession(t *testing.T) {
	f := newFakeConsole(t)
	_, _, acc := newTestHandler(t, f)
	quota := acc.GrokConsoleQuota
	testutil.Equal(t, quota.BudgetCents, int64(500))
	testutil.Equal(t, quota.UsedCents, int64(120))
	testutil.False(t, quota.Unlinked)
	testutil.False(t, quota.Exhausted())
	fields := map[string]any{}
	ProjectQuota(fields, acc)
	testutil.Equal(t, fields["quota_unit"], any("usd"))
	testutil.Equal(t, fields["quota_limit"], any(5.0))
	testutil.Equal(t, fields["quota_remaining"], any(3.8))
}

func TestConsoleUnlinkedAccountsAreWithheld(t *testing.T) {
	f := newFakeConsole(t)
	f.unlinked = true
	h, _, acc := newTestHandler(t, f)
	testutil.True(t, acc.GrokConsoleQuota.Unlinked)
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(`{"model":"console/grok-4.5","messages":[{"role":"user","content":"hi"}]}`)))
	testutil.Equal(t, rec.Code, http.StatusServiceUnavailable)
	// No inference reached the upstream.
	testutil.Equal(t, f.lastPath, "/v1/models")
}

func TestConsoleSpentPoolIsRecoveredAtItsReset(t *testing.T) {
	f := newFakeConsole(t)
	f.usage = `{"weeklyIncludedUsage":{"budgetCents":500,"usedCents":"650","resetsAt":{"seconds":4102444800}}}`
	h, st, acc := newTestHandler(t, f)
	testutil.True(t, acc.GrokConsoleQuota.Exhausted(), "overspend still reads as spent")
	testutil.Equal(t, acc.GrokConsoleQuota.ResetAt, time.Unix(4102444800, 0).UTC())
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(`{"model":"console/grok-4.5","messages":[{"role":"user","content":"hi"}]}`)))
	testutil.Equal(t, rec.Code, http.StatusServiceUnavailable)

	// The reset passed and the dashboard shows a fresh week: maintenance
	// re-reads it on the recovery schedule, without waiting out its cadence.
	t.Cleanup(func() { allowanceRecovery.Done(acc.ID) })
	acc.GrokConsoleQuota.ResetAt = time.Now().Add(-time.Minute)
	testutil.NoError(t, st.UpdateAccount(context.Background(), acc))
	f.usage = `{"weeklyIncludedUsage":{"budgetCents":500}}`
	maintainWith(context.Background(), h.client, st, acc)
	testutil.False(t, acc.GrokConsoleQuota.Exhausted())
	testutil.Equal(t, acc.GrokConsoleQuota.UsedCents, int64(0))
	testutil.False(t, allowanceRecovery.Pending(acc.ID))
}

func TestParseUsageStatusRejectsAMissingBudget(t *testing.T) {
	_, err := parseUsageStatus([]byte(`{"weeklyIncludedUsage":{"usedCents":1}}`))
	testutil.True(t, err != nil)
	_, err = parseUsageStatus([]byte(`{}`))
	testutil.True(t, err != nil)
	testutil.False(t, allowedWorkOSHost(&url.URL{Scheme: "https", Host: "evil.example"}, "grok.com"))
	testutil.True(t, allowedWorkOSHost(&url.URL{Scheme: "https", Host: "authenticator.cursor.sh"}, "grok.com"))
}
