package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/modelrefresh"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

func decodeQoderBodyForTest(encoded []byte) ([]byte, error) {
	if strings.ContainsAny(string(encoded), "\r\n") {
		return nil, fmt.Errorf("encoded body contains a line break")
	}
	const privateAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	const standardAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	q := len(encoded) / 3
	swapped := make([]byte, 0, len(encoded))
	swapped = append(swapped, encoded[len(encoded)-q:]...)
	swapped = append(swapped, encoded[q:len(encoded)-q]...)
	swapped = append(swapped, encoded[:q]...)
	for i, b := range swapped {
		switch b {
		case '$':
			swapped[i] = '='
		default:
			idx := strings.IndexByte(privateAlphabet, b)
			if idx < 0 {
				return nil, fmt.Errorf("invalid private alphabet byte %q", b)
			}
			swapped[i] = standardAlphabet[idx]
		}
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(swapped)))
	n, err := base64.StdEncoding.Decode(decoded, swapped)
	if err != nil {
		return nil, err
	}
	return decoded[:n], nil
}

// qoderE2EStub serves every endpoint the Qoder channel touches, so a request can
// travel the real route table, the real handler and the real client.
type qoderE2EStub struct {
	*httptest.Server
	chatHeaders    http.Header
	chatBody       []byte
	chatCalls      int
	modelListCalls int
	polls          int
}

func newQoderE2EStub(t *testing.T) *qoderE2EStub {
	t.Helper()
	stub := &qoderE2EStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/deviceToken/poll":
			stub.polls++
			if stub.polls < 2 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"token":"access-1","refresh_token":"refresh-1","expires_in":86400,"user_id":"uid-e2e","user_name":"e2e"}`))
		case "/api/v1/userinfo":
			_, _ = w.Write([]byte(`{"uid":"uid-e2e","name":"e2e","email":"e2e@example.com"}`))
		case "/algo/api/v2/model/list":
			testutil.CheckEqual(t, r.Method, http.MethodGet)
			testutil.CheckEqual(t, r.URL.RawQuery, "Encode=1")
			// The catalog is now read from the signed control plane, so the
			// refresh depends on this route answering with the account's models.
			testutil.CheckFalsef(t, r.Header.Get("Cosy-Key") == "" || r.Header.Get("Cosy-MachineId") == "", "model list request is missing the derived auth chain: %v", r.Header)
			auth := r.Header.Get("Authorization")
			testutil.CheckFalsef(t, !strings.HasPrefix(auth, "Bearer COSY."), "model list Authorization = %q, want a COSY bearer", auth)
			stub.modelListCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{"chat":[` +
				`{"key":"qmodel_latest","name":"Qwen3.7-Max","display_name":"Qwen3.7-Max","format":"openai","source":"system","enable":true,"is_reasoning":false,"max_input_tokens":1000000},` +
				`{"key":"dmodel","name":"DeepSeek-V4-Pro","display_name":"DeepSeek-V4-Pro","format":"openai","source":"system","enable":true,"is_reasoning":true,"max_input_tokens":1000000}` +
				`]}}`))
		case "/algo/api/v2/service/pro/sse/agent_chat_generation":
			stub.chatCalls++
			stub.chatHeaders = r.Header.Clone()
			body := make([]byte, 1<<20)
			n, _ := r.Body.Read(body)
			stub.chatBody = body[:n]
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sseEnvelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"e2e answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)))
			_, _ = w.Write([]byte("event:finish\ndata: {}\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	return stub
}

func sseEnvelope(inner string) string {
	raw, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner})
	return "data: " + string(raw) + "\n\n"
}

// TestQoderChannelEndToEnd drives one chat completion through the registered
// route table with a Qoder account that the login flow created.
//
// This is the integration the unit tests cannot reach: route table, channel
// detection, account selection, client construction, the derived authentication
// chain and the SSE conversion all run together. The requests go through a real
// HTTP server rather than a bare recorder, so the same request lifecycle the
// deployment uses is exercised (and the -race detector observes it).
func TestQoderChannelEndToEnd(t *testing.T) {

	stub := newQoderE2EStub(t)
	defer stub.Close()

	// Inference auth is unconditional, so the channel request carries a managed
	// key; the admin request uses the admin token as before.
	const managedKey = "sk-qoder-e2e"
	cfg := &config.Config{
		AdminUser:           "admin",
		AdminPass:           "secret",
		AdminToken:          "admintoken",
		AdminPath:           "/admin",
		QoderOAuthBaseURL:   stub.URL,
		QoderOpenAPIBaseURL: stub.URL,
		QoderInferenceURL:   stub.URL,
	}

	// Build the client through the same provider table the server uses, so the
	// request path exercises the real seam.
	e := newChannelE2E(t, "qoder-e2e:", managedKey, cfg)

	// 1. Create the account through the device authorization flow.
	startResp := e.do(t, http.MethodPost, "/api/qoder/login", "", true)
	startBody := e.readBody(t, startResp)
	testutil.Equal(t, startResp.StatusCode, http.StatusOK)
	var started struct {
		ID                      string `json:"id"`
		VerificationURIComplete string `json:"verification_uri_complete"`
	}
	err := json.Unmarshal([]byte(startBody), &started)
	testutil.Falsef(t, err != nil || started.ID == "", "login start response = %q", startBody)
	testutil.MustContain(t, started.VerificationURIComplete, "/device/selectAccounts?")

	var final struct {
		Status    string `json:"status"`
		AccountID int64  `json:"account_id"`
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		pollResp := e.do(t, http.MethodGet, "/api/qoder/login/"+started.ID, "", true)
		pollBody := e.readBody(t, pollResp)
		testutil.Equal(t, pollResp.StatusCode, http.StatusOK)
		testutil.NoError(t, json.Unmarshal([]byte(pollBody), &final), "decode poll: %v")
		if final.Status == "complete" || final.Status == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	testutil.Equal(t, final.Status, "complete")

	// 2. Refresh the channel catalog from the account.
	refreshResp := e.do(t, http.MethodPost, "/api/models/refresh?channel=qoder", "", true)
	refreshBody := e.readBody(t, refreshResp)
	testutil.Equal(t, refreshResp.StatusCode, http.StatusOK)
	var refreshed modelrefresh.Result
	testutil.NoError(t, json.Unmarshal([]byte(refreshBody), &refreshed), "decode refresh: %v")
	testutil.Falsef(t, refreshed.Channel != "Qoder" || refreshed.Discovered == 0, "refresh result = %+v, want a discovered Qoder catalog", refreshed)
	// The catalog must have come from the signed upstream read, not from a
	// compiled-in list.
	testutil.Equal(t, refreshed.Source, "qoder_upstream_models")
	testutil.NotEqual(t, stub.modelListCalls, 0)
	testutil.Equal(t, refreshed.Verified, refreshed.Discovered)

	// 3. Run one chat completion through the channel route.
	chatResp := e.do(t, http.MethodPost, "/qoder/v1/chat/completions",
		`{"model":"Qwen3.7-Max","stream":true,"messages":[{"role":"user","content":"hello"}]}`, false)
	chatBody := e.readBody(t, chatResp)
	testutil.Equal(t, chatResp.StatusCode, http.StatusOK)
	testutil.MustContain(t, chatBody, "e2e answer")
	testutil.Equal(t, stub.chatCalls, 1)

	// The upstream request must carry the derived authentication chain, not the
	// bare device token.
	for _, header := range []string{"Authorization", "Cosy-Key", "Cosy-MachineId", "Cosy-MachineToken", "Cosy-User", "Cosy-Date", "Cosy-Scene", "Cosy-Data-Policy", "Login-Version", "X-Model-Key", "X-Model-Source"} {
		testutil.CheckNotEqual(t, stub.chatHeaders.Get(header), "")
	}
	auth := stub.chatHeaders.Get("Authorization")
	testutil.CheckFalsef(t, !strings.HasPrefix(auth, "Bearer COSY."), "Authorization = %q, want a COSY bearer", auth)
	testutil.CheckEqual(t, stub.chatHeaders.Get("X-Model-Key"), "qmodel_latest")
	testutil.CheckEqual(t, stub.chatHeaders.Get("Cosy-User"), "uid-e2e")

	// The device token must never appear in a header: the runtime fields are the
	// request credential.
	testutil.MustNotContain(t, fmtHeaders(stub.chatHeaders), "access-1")

	// The body is in the private encoding and carries the chat contract.
	testutil.NotEqual(t, len(stub.chatBody), 0)
	decoded, err := decodeQoderBodyForTest(stub.chatBody)
	testutil.NoError(t, err, "DecodeBody() error = %v")
	// The capture shows the QoderWork client sending no account class: the field
	// is present and empty rather than omitted.
	for _, want := range []string{`"chat_task":"FREE_INPUT"`, `"session_type":"qoder_work"`, `"agent_id":"agent_common"`, `"stream":true`, `"aliyun_user_type":""`} {
		testutil.CheckContain(t, string(decoded), want)
	}

	// 4. The account row must not expose either secret.
	redactResp := e.do(t, http.MethodGet, "/api/accounts", "", true)
	redactBody := e.readBody(t, redactResp)
	testutil.Equal(t, redactResp.StatusCode, http.StatusOK)
	testutil.MustNotContain(t, redactBody, "refresh-1")
	testutil.MustNotContain(t, redactBody, "runtime-key")
}

func fmtHeaders(headers http.Header) string {
	var builder strings.Builder
	for name, values := range headers {
		builder.WriteString(name)
		builder.WriteString(":")
		builder.WriteString(strings.Join(values, " "))
		builder.WriteString("\n")
	}
	return builder.String()
}
