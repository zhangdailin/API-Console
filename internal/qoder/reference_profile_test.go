package qoder

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// referenceBridgeBody is the body the reference bridge (EchoPing07/Qoder-2API-Go
// at 1ccd617) builds for a single "hello" to qfmodel, produced by running its
// own newRequestBody/buildParameters/BuildQoderMessages. begin_at is dropped:
// it is a timestamp.
const referenceBridgeBody = `{
  "request_id": "req-1", "request_set_id": "set-1", "chat_record_id": "req-1",
  "stream": true, "chat_task": "FREE_INPUT",
  "chat_context": {
    "extra": {"modelConfig": {"is_reasoning": false, "key": "qfmodel"},
              "originalContent": {"type": "text", "text": "hello"}},
    "text": {"type": "text", "text": "hello"}
  },
  "session_id": "sess", "source": 1, "version": "3",
  "aliyun_user_type": "personal_professional_trial", "session_type": "qodercli",
  "agent_id": "agent_common", "task_id": "common",
  "model_config": {"key": "qfmodel", "is_vl": false, "is_reasoning": false, "source": "system"},
  "messages": [{
    "role": "user", "content": "",
    "response_meta": {"id": "", "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
      "completion_tokens_details": {"reasoning_tokens": 0}, "prompt_tokens_details": {"cached_tokens": 0}}},
    "reasoning_content_signature": "",
    "contents": [{"type": "text", "text": "hello"}]
  }],
  "business": {"id": "set-1", "name": "hello"},
  "parameters": {"max_tokens": 32000}
}`

func TestReferenceBodyMatchesReferenceBridge(t *testing.T) {
	enabled := true
	model := modelEntry{Key: "qfmodel", Name: "Qwen3.8-Flash", Format: "openai", Source: "system", Enable: &enabled}
	req := upstream.UpstreamRequest{Model: "qwen3.8-flash", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}}}
	encoded, err := buildChatBodyProfile(req, model, "sess", "req-1", "set-1", DefaultClientVersion, "", sceneBusinessProduct)
	testutil.NoError(t, err)
	raw, err := decodeBody(encoded)
	testutil.NoError(t, err)
	raw, err = referenceBody(raw, "personal_professional_trial")
	testutil.NoError(t, err)

	var got, want map[string]any
	testutil.NoError(t, json.Unmarshal(raw, &got))
	testutil.NoError(t, json.Unmarshal([]byte(referenceBridgeBody), &want))
	delete(got["business"].(map[string]any), "begin_at")
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		t.Fatalf("reference body differs\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

func TestReferenceRuntimeInputLayout(t *testing.T) {
	acc := signedTestAccount()
	acc.QoderUserName = "name-1"
	acc.QoderOrganizationID = "org-1"
	acc.QoderQuota.UserType = "personal_standard"
	client := NewFromAccount(acc, nil)
	raw, err := marshalNoEscape(client.referenceRuntimeInput(client.currentCredentials()))
	testutil.NoError(t, err)
	// Key order and the empty keys are the reference bridge's authPayloadStruct.
	testutil.Equal(t, string(raw), `{"name":"name-1","aid":"uid-1","uid":"uid-1","yx_uid":"","organization_id":"org-1","organization_name":"","user_type":"personal_standard","security_oauth_token":"access-1","refresh_token":"refresh-1"}`)
}

// TestReferenceProfileSendsTheReferenceRequest drives a chat through the
// reference profile, including a 401 that forces a refresh and a replay, and
// checks every request is the reference bridge's request.
func TestReferenceProfileSendsTheReferenceRequest(t *testing.T) {
	var mu sync.Mutex
	var headers []http.Header
	var bodies []map[string]any
	var payloads []map[string]string
	chats := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "deviceToken/refresh") {
			_, _ = io.WriteString(w, `{"device_token":"access-new","refresh_token":"refresh-new","expires_in":7200}`)
			return
		}
		encoded, _ := io.ReadAll(r.Body)
		raw, err := decodeBody(encoded)
		testutil.NoError(t, err)
		var body map[string]any
		testutil.NoError(t, json.Unmarshal(raw, &body))
		encodedPayload := strings.SplitN(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer COSY."), ".", 2)[0]
		decoded, _ := base64.StdEncoding.DecodeString(encodedPayload)
		var payload map[string]string
		_ = json.Unmarshal(decoded, &payload)
		mu.Lock()
		headers, bodies, payloads = append(headers, r.Header.Clone()), append(bodies, body), append(payloads, payload)
		chats++
		first := chats == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"unauthorized"}`)
			return
		}
		_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`)+"event:finish\ndata: {}\n\n")
	}))
	defer server.Close()

	acc := signedTestAccount()
	acc.QoderQuota.UserType = "personal_professional_trial"
	client := NewFromAccount(acc, &config.Config{QoderRequestProfile: "reference"})
	setTestEndpoints(client, server.URL, server.URL, server.URL)
	var text strings.Builder
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "Qwen3.7-Max",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, func(m upstream.SSEMessage) {
		if delta, ok := m.Event["delta"].(string); ok {
			text.WriteString(delta)
		}
	}, nil)
	testutil.NoError(t, err)
	testutil.Equal(t, text.String(), "OK")
	testutil.Equal(t, len(bodies), 2)

	wantHeaders := []string{"Accept", "Accept-Encoding", "Authorization", "Cache-Control", "Content-Length", "Content-Type",
		"Cosy-Clienttype", "Cosy-Data-Policy", "Cosy-Date", "Cosy-Key", "Cosy-Machineid", "Cosy-Machinetoken",
		"Cosy-Machinetype", "Cosy-User", "Cosy-Version", "Login-Version", "User-Agent", "X-Model-Key", "X-Model-Source"}
	for i := range bodies {
		keys := make([]string, 0, len(headers[i]))
		for key := range headers[i] {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		testutil.Equal(t, strings.Join(keys, ","), strings.Join(wantHeaders, ","))
		testutil.Equal(t, headers[i].Get("Cosy-Clienttype"), "5")
		testutil.Equal(t, headers[i].Get("Cosy-Version"), referenceCosyVersion)
		testutil.Equal(t, headers[i].Get("User-Agent"), referenceUserAgent)
		testutil.Equal(t, len(headers[i].Get("Cosy-Machinetoken")), 50)
		testutil.Equal(t, len(headers[i].Get("Cosy-Machinetype")), 18)
		testutil.Equal(t, payloads[i]["cosyVersion"], referenceCosyVersion)
		testutil.False(t, payloads[i]["requestId"] == bodies[i]["request_id"], "the COSY payload reused the body request id")
		testutil.Equal(t, bodies[i]["session_type"], "qodercli")
		testutil.Equal(t, bodies[i]["aliyun_user_type"], "personal_professional_trial")
		message := bodies[i]["messages"].([]any)[0].(map[string]any)
		testutil.Equal(t, message["content"], "")
		testutil.False(t, message["contents"] == nil || message["response_meta"] == nil, "message is not in the reference shape")
		_, hasRetry := bodies[i]["is_retry"]
		testutil.False(t, hasRetry, "a QoderWork-only field survived")
	}
	// The replay after the refresh is a new attempt of the same task, signed
	// with a pair derived for the rotated token.
	testutil.False(t, bodies[0]["request_id"] == bodies[1]["request_id"], "the replay reused the refused request id")
	testutil.Equal(t, bodies[0]["request_set_id"], bodies[1]["request_set_id"])
	testutil.False(t, headers[0].Get("Cosy-Key") == headers[1].Get("Cosy-Key"), "the rotated token kept the previous runtime pair")
}

func TestRequestProfileDefaultsToQoderWork(t *testing.T) {
	testutil.Equal(t, resolveRequestProfile(""), RequestProfileQoderWork)
	testutil.Equal(t, resolveRequestProfile("unknown"), RequestProfileQoderWork)
	testutil.Equal(t, resolveRequestProfile(" Reference "), RequestProfileReference)
	testutil.Equal(t, NewFromAccount(signedTestAccount(), nil).profile, RequestProfileQoderWork)
	testutil.Equal(t, NewFromAccount(signedTestAccount(), &config.Config{}).profile, RequestProfileQoderWork)
}
