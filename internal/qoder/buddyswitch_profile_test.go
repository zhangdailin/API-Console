package qoder

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// buddySwitchTemplateKeys are the top-level keys of buddy-switch's CLI
// template (baseprompt.json at e664b38), which build_chat_body fills in.
var buddySwitchTemplateKeys = []string{
	"agent_id", "aliyun_user_type", "business", "chat_context", "chat_prompt", "chat_record_id",
	"chat_task", "code_language", "image_urls", "is_reply", "is_retry", "messages", "model_config",
	"parameters", "request_id", "request_set_id", "session_id", "session_type", "source", "stream",
	"task_id", "tools", "version",
}

func TestBuddySwitchBodyHasTheTemplateShape(t *testing.T) {
	enabled := true
	model := modelEntry{Key: "qfmodel", Name: "Qwen3.8-Flash", Format: "openai", Source: "system", Enable: &enabled}
	req := upstream.UpstreamRequest{
		Model:    "qwen3.8-flash",
		System:   []prompt.SystemItem{{Type: "text", Text: "be brief"}},
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}
	encoded, err := buildChatBodyProfile(req, model, "sess", "req-1", "set-1", DefaultClientVersion, "", sceneBusinessProduct)
	testutil.NoError(t, err)
	raw, err := decodeBody(encoded)
	testutil.NoError(t, err)
	raw, err = buddySwitchBody(raw, "")
	testutil.NoError(t, err)
	var body map[string]any
	testutil.NoError(t, json.Unmarshal(raw, &body))

	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	testutil.Equal(t, strings.Join(keys, ","), strings.Join(buddySwitchTemplateKeys, ","))
	testutil.Equal(t, body["session_type"], any("qodercli"))
	testutil.Equal(t, body["aliyun_user_type"], any(buddySwitchDefaultUserType))
	business := body["business"].(map[string]any)
	testutil.Equal(t, business["product"], any("cli"))
	testutil.Equal(t, business["version"], any(buddySwitchBusinessVersion))
	testutil.Equal(t, business["stage"], any("start"))
	_, hasSubTask := business["sub_task"]
	testutil.False(t, hasSubTask, "a QoderWork business field survived")
	chatContext := body["chat_context"].(map[string]any)
	testutil.Equal(t, chatContext["text"].(map[string]any)["text"], any("hello"))
	messages := body["messages"].([]any)
	testutil.Equal(t, messages[0].(map[string]any)["role"], any("system"))
	testutil.Equal(t, messages[0].(map[string]any)["content"], any("be brief"))
}

func TestBuddySwitchIdentityAndDevice(t *testing.T) {
	acc := signedTestAccount()
	acc.QoderUserName = "name-1"
	client := NewFromAccount(acc, nil)
	raw, err := client.cliRuntimePlaintext(client.currentCredentials(), RequestProfileBuddySwitch)
	testutil.NoError(t, err)
	testutil.Equal(t, string(raw), `{"aid":"uid-1","name":"name-1","organization_id":"","organization_name":"","refresh_token":"refresh-1","security_oauth_token":"access-1","uid":"uid-1","user_type":"personal_professional_trial","yx_uid":""}`)
	// Vectors computed independently from buddy-switch's device.rs algorithm.
	testutil.Equal(t, buddySwitchMachineToken("uid-1"), "32Ugo4rViVtY4Ssqidb_KmmueV0j5CqfTmMtmmIuj-4")
	testutil.Equal(t, buddySwitchMachineType("uid-1"), "8a96d80e2a78b00839")
}

func TestBuddySwitchProfileSendsItsRequest(t *testing.T) {
	var header http.Header
	var body map[string]any
	var payload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoded, _ := io.ReadAll(r.Body)
		raw, _ := decodeBody(encoded)
		_ = json.Unmarshal(raw, &body)
		header = r.Header.Clone()
		part := strings.SplitN(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer COSY."), ".", 2)[0]
		decoded, _ := base64.StdEncoding.DecodeString(part)
		_ = json.Unmarshal(decoded, &payload)
		_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`)+"event:finish\ndata: {}\n\n")
	}))
	defer server.Close()

	client := NewFromAccount(signedTestAccount(), &config.Config{QoderRequestProfile: "buddyswitch"})
	setTestEndpoints(client, server.URL, server.URL, server.URL)
	testutil.NoError(t, client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "Qwen3.7-Max", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, nil, nil))
	testutil.Equal(t, header.Get("Cosy-Version"), buddySwitchCosyVersion)
	testutil.Equal(t, header.Get("Cosy-Clientip"), buddySwitchClientIP)
	testutil.Equal(t, header.Get("Cosy-Clienttype"), "5")
	testutil.Equal(t, header.Get("User-Agent"), "Go-http-client/2.0")
	testutil.Equal(t, header.Get("Cosy-Machineid"), signedTestAccount().QoderMachineID)
	testutil.Equal(t, header.Get("Cosy-Machinetype"), buddySwitchMachineType("uid-1"))
	testutil.Equal(t, payload["cosyVersion"], buddySwitchCosyVersion)
	testutil.Equal(t, body["session_type"], any("qodercli"))
	testutil.Equal(t, body["business"].(map[string]any)["product"], any("cli"))
}

func TestBuddySwitchPrefersApi1(t *testing.T) {
	client := NewFromAccount(signedTestAccount(), &config.Config{QoderRequestProfile: "buddyswitch"})
	hosts := client.cliHostOrder(client.endpoints.inferenceHosts())
	testutil.Equal(t, strings.Join(hosts, ","), "https://api1.qoder.sh,https://api2.qoder.sh,https://api3.qoder.sh")
	// The default profile keeps api2 first.
	plain := NewFromAccount(signedTestAccount(), nil)
	testutil.Equal(t, plain.cliHostOrder(plain.endpoints.inferenceHosts())[0], DefaultInferenceURL)
}
