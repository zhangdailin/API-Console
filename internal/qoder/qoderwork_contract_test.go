package qoder

import (
	"encoding/json"
	"strings"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// The contract pinned below comes from a packet capture of the international
// QoderWork client. Every value was observed on the wire. Moving one of them
// changes what the gateway is asked to accept, which is not something to do
// without new evidence.

// TestQoderWorkIdentityIsTheOnlyDialect checks that the client never falls back to the
// IDE emulation this channel was built around (client type 5, scene
// "assistant", session_type "qoder", product "ide").
func TestQoderWorkIdentityIsTheOnlyDialect(t *testing.T) {
	t.Parallel()

	testutil.Equal(t, sceneClientID, "6")
	testutil.Equal(t, sceneName, "qwork")
	testutil.Equal(t, sessionType, "qoder_work")
	testutil.Equal(t, sceneBusinessProduct, "qoder_work")
	testutil.Equal(t, DefaultClientVersion, "1.0.45")
	// The capture reports client type 6 and machine type 5 in one request, so
	// these cannot share a constant.
	testutil.Equal(t, machineSceneType, "5")
}

// TestQoderWorkBodyMatchesCapture pins the request body field for field. The
// top-level key set matters as much as the values: this channel used to send
// image_urls, code_language, chat_prompt and custom_model, none of which the
// captured client sends.
func TestQoderWorkBodyMatchesCapture(t *testing.T) {
	t.Parallel()

	model := modelEntry{Key: "qfmodel", DisplayName: "Qwen3.8-Flash", IsReasoning: true, IsVL: true, MaxInputTokens: 180000}
	req := upstream.UpstreamRequest{Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}}
	encoded, err := buildChatBodyProfile(req, model, "session", "request", "request-set", DefaultClientVersion, "", sceneBusinessProduct)
	testutil.NoError(t, err)
	raw, err := decodeBodyForTest(encoded)
	testutil.NoError(t, err)
	var body map[string]interface{}
	testutil.NoError(t, json.Unmarshal(raw, &body))

	wantKeys := []string{
		"agent_id", "aliyun_user_type", "business", "chat_context",
		"chat_record_id", "chat_task", "is_reply", "is_retry", "messages",
		"model_config", "parameters", "request_id", "request_set_id",
		"session_id", "session_type", "source", "stream", "system",
		"task_id", "tools", "version",
	}
	testutil.Equal(t, len(body), len(wantKeys))
	for _, key := range wantKeys {
		_, present := body[key]
		testutil.Falsef(t, !present, "body is missing %q, which the capture carries", key)
	}

	for key, want := range map[string]interface{}{
		"stream":           true,
		"chat_task":        "FREE_INPUT",
		"is_reply":         true,
		"is_retry":         false,
		"source":           float64(1),
		"version":          "3",
		"agent_id":         "agent_common",
		"task_id":          "common",
		"session_type":     "qoder_work",
		"aliyun_user_type": "",
	} {
		testutil.CheckEqual(t, body[key], want)
	}

	// request_id and chat_record_id carry the attempt; request_set_id carries
	// the task. The capture shows them as two different values.
	testutil.Equal(t, body["request_id"], "request")
	testutil.Equal(t, body["chat_record_id"], "request")
	testutil.Equal(t, body["request_set_id"], "request-set")

	business := body["business"].(map[string]interface{})
	testutil.Equal(t, len(business), 8)
	for key, want := range map[string]interface{}{
		"product":  "qoder_work",
		"version":  DefaultClientVersion,
		"type":     "agent",
		"id":       "request-set",
		"stage":    "start",
		"sub_task": "ws_builtin_general",
	} {
		testutil.CheckEqual(t, business[key], want)
	}

	// is_reasoning follows the model's own catalog capability.
	modelConfig := body["model_config"].(map[string]interface{})
	testutil.Equal(t, modelConfig["is_reasoning"], true)
	testutil.Equal(t, modelConfig["is_vl"], true)
	testutil.Equal(t, modelConfig["source"], "system")

	// text and extra.originalContent are the same plain string in the capture.
	context := body["chat_context"].(map[string]interface{})
	testutil.Equal(t, context["text"], "hi")
	testutil.Equal(t, context["chatPrompt"], "")
	extra := context["extra"].(map[string]interface{})
	testutil.Equal(t, extra["originalContent"], "hi")
	testutil.Equal(t, extra["modelConfig"].(map[string]interface{})["is_reasoning"], true)

	testutil.EqualAny(t, body["parameters"].(map[string]interface{})["max_tokens"], float64(32000))
}

// TestQoderWorkStreamFramesParse covers the envelope shape the gateway actually
// sends: body is a JSON *string* holding an OpenAI chunk, terminated by an
// event:finish line. The observed capture parses to one reasoning delta.
func TestQoderWorkStreamFramesParse(t *testing.T) {
	t.Parallel()

	frame := `{"headers":{"Content-Type":["application/json"]},"body":"{\"choices\":[{\"delta\":{\"content\":\"\",\"reasoning_content\":\"hi\",\"role\":\"assistant\"},\"index\":0}],\"created\":1,\"id\":\"chatcmpl-1\",\"model\":\"auto\",\"object\":\"chat.completion.chunk\"}","statusCodeValue":200,"statusCode":"OK"}`
	stream := "data:" + frame + "\n\n" + "event:finish\n\n"

	types := map[string]int{}
	_, err := consumeStreamObserved(strings.NewReader(stream), false, func(m upstream.SSEMessage) { types[m.Type]++ }, nil)
	testutil.CheckNoError(t, err)
	testutil.Equal(t, types["model.reasoning-delta"], 1)
}

// TestArrayShapedRefusalStaysDiagnosable covers the failure that took the
// channel down. The gateway answered a rejected request with a JSON array in
// body, and the error collapsed into "unsupported stream format" with the
// upstream's own explanation discarded.
func TestArrayShapedRefusalStaysDiagnosable(t *testing.T) {
	t.Parallel()

	frame := `{"headers":{},"body":"[{\"code\":\"10605\",\"message\":\"service not available\"}]","statusCodeValue":200,"statusCode":"OK"}`
	stream := "data:" + frame + "\n\n"

	_, err := consumeStreamObserved(strings.NewReader(stream), false, func(upstream.SSEMessage) {}, nil)
	testutil.False(t, err == nil, "an array-shaped body was accepted as a chunk")
	testutil.MustContain(t, err.Error(), "10605")
}

// TestNotificationsControlFrameDoesNotAbortTheStream covers the frame that broke
// the QoderWork identity in production. The gateway prefixes advisory notices
// and multiplexes them into the chunk channel; parsing one as a chunk aborted an
// otherwise healthy reply and reported it as an unsupported stream format.
func TestNotificationsControlFrameDoesNotAbortTheStream(t *testing.T) {
	t.Parallel()

	notice := `{"headers":{},"body":"[NOTIFICATIONS]#{\"notifications\":[{\"extras\":{\"pricingUrl\":\"https://qoder.com/pricing?client=qoder\",\"nextResetAt\":1791397354935},\"isHighestTier\":false,\"notificationType\":\"quota_low\"}]}","statusCodeValue":200,"statusCode":"OK"}`
	answer := `{"headers":{"Content-Type":["application/json"]},"body":"{\"choices\":[{\"delta\":{\"content\":\"OK\",\"role\":\"assistant\"},\"index\":0}],\"created\":1,\"id\":\"chatcmpl-1\",\"model\":\"auto\",\"object\":\"chat.completion.chunk\"}","statusCodeValue":200,"statusCode":"OK"}`
	stream := "data:" + notice + "\n\n" + "data:" + answer + "\n\n" + "event:finish\n\n"

	var text strings.Builder
	res, err := consumeStreamObserved(strings.NewReader(stream), false, func(m upstream.SSEMessage) {
		if m.Type == "model.text-delta" {
			if delta, ok := m.Event["delta"].(string); ok {
				text.WriteString(delta)
			}
		}
	}, nil)
	testutil.NoError(t, err, "a notifications control frame aborted the stream: %v")
	testutil.Equal(t, res.ControlFrames["NOTIFICATIONS"], 1)
	testutil.Equal(t, text.String(), "OK")
}
