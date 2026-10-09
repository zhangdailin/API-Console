package grok

import (
	"encoding/json"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
)

func TestNativeResponsesRelayPreservesNullError(t *testing.T) {
	response := map[string]interface{}{
		"id": "resp_ok", "object": "response", "created_at": 42,
		"model": "grok-4.7", "status": "completed", "error": nil,
		"output": []interface{}{map[string]interface{}{
			"id": "msg_ok", "type": "message", "role": "assistant", "status": "completed",
			"content": []interface{}{map[string]interface{}{"type": "output_text", "text": "2", "annotations": []interface{}{}}},
		}},
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, contentType := range []string{"application/json", "text/event-stream"} {
		t.Run(contentType, func(t *testing.T) {
			input := string(raw)
			if contentType == "text/event-stream" {
				input = parityFrame("response.completed", map[string]interface{}{"id": "resp_ok", "response": response})
			}
			recorder := httptest.NewRecorder()
			id, captured, result := copyNativeCLIResponseAndCaptureModel(recorder, strings.NewReader(input), contentType, "grok-4.7")
			if result.Err != nil || result.Finish != "stop" || id != "resp_ok" {
				t.Fatalf("id=%q, outcome=%+v", id, result)
			}
			for _, output := range []string{recorder.Body.String(), string(captured)} {
				if !strings.Contains(output, `"error":null`) || strings.Contains(output, upstreamRejectionCode) || !strings.Contains(output, `"text":"2"`) {
					t.Fatalf("successful response or audit capture changed: %s", output)
				}
			}
		})
	}
}

// A rejection and a transient failure must not reach the client as the same
// event: the first is not worth retrying and the second is.
func TestRedactResponseErrorSeparatesRejectionFromFailure(t *testing.T) {
	cases := []struct {
		name        string
		event       map[string]interface{}
		wantCode    string
		wantMessage string
		wantField   string
	}{
		{
			name: "model rejection keeps a distinguishable code",
			event: map[string]interface{}{
				"error": map[string]interface{}{
					"message": "The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account.",
					"type":    "invalid_request_error",
				},
			},
			wantCode:    upstreamRejectionCode,
			wantMessage: "The upstream rejected the request parameters or model. Check the request and model selection.",
			wantField:   "error",
		},
		{
			name:        "model not found is a rejection",
			event:       map[string]interface{}{"error": map[string]interface{}{"message": "model not found"}},
			wantCode:    upstreamRejectionCode,
			wantMessage: "The upstream rejected the request parameters or model. Check the request and model selection.",
			wantField:   "error",
		},
		{
			name: "server failure keeps the generic code",
			event: map[string]interface{}{
				"error": map[string]interface{}{"message": "Our servers are currently overloaded. Please try again later."},
			},
			wantCode:    "upstream_error",
			wantMessage: "The upstream request failed. Use the request ID to inspect diagnostics.",
			wantField:   "error",
		},
		{
			name: "bare error frame carries code and message",
			event: map[string]interface{}{
				"type":    "error",
				"message": "model not found",
				"code":    "some_upstream_code",
				"param":   "model",
			},
			// An error frame has no nested envelope: the code and message stay on
			// the event itself, and the upstream-only fields are dropped.
			wantCode:    upstreamRejectionCode,
			wantMessage: "The upstream rejected the request parameters or model. Check the request and model selection.",
		},
		{
			name:  "empty error object still names the rejection",
			event: map[string]interface{}{"error": map[string]interface{}{}},
			// The message is unclassifiable, so the constant stands in for it; the
			// code is what a client acts on.
			wantCode:    upstreamRejectionCode,
			wantMessage: upstreamRejectionMessage,
			wantField:   "error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutil.False(t, !redactResponseError(tc.event), "redactResponseError() = false, want true")
			envelope := tc.event
			if tc.wantField != "" {
				nested, ok := tc.event[tc.wantField].(map[string]interface{})
				testutil.True(t, ok, "%s envelope = %#v")
				envelope = nested
			}
			testutil.Equal(t, chatwire.ParseLooseStringAny(envelope["code"]), tc.wantCode)
			testutil.Equal(t, chatwire.ParseLooseStringAny(envelope["message"]), tc.wantMessage)
			_, leaked := tc.event["param"]
			testutil.Falsef(t, leaked, "error frame leaked the upstream param field: %#v", tc.event)
			_, leaked = tc.event["code"]
			testutil.Falsef(t, tc.wantField == "error" && leaked, "file error envelope wrote the code to the outer event: %#v", tc.event)
		})
	}
}

// The upstream's own error text must never reach the client.
func TestRedactResponseErrorNeverForwardsUpstreamText(t *testing.T) {
	secret := "sk-live-upstream-credential"
	event := map[string]interface{}{
		"error": map[string]interface{}{
			"message": "request failed with " + secret + " (status=400)",
			"code":    "invalid_request_error",
		},
	}
	redactResponseError(event)
	encoded := event["error"].(map[string]interface{})
	testutil.MustNotContain(t, chatwire.ParseLooseStringAny(encoded["message"]), secret)
	got := codeForCategory("client")
	testutil.Falsef(t, chatwire.ParseLooseStringAny(encoded["code"]) != got, "code = %q, want %q", chatwire.ParseLooseStringAny(encoded["code"]), got)
}

// A failed response envelope is normalized so the message inside it cannot
// disagree with the event that carried it.
func TestRedactResponseErrorNormalizesFailedEnvelope(t *testing.T) {
	event := map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"id": "resp_1", "object": "response", "status": "failed", "model": "grok-4.6",
			"error": map[string]interface{}{
				"code":    "upstream_stream_error",
				"message": "grok upstream stream error: model not found",
			},
		},
	}
	redactResponseError(event)
	response := event["response"].(map[string]interface{})
	envelope := response["error"].(map[string]interface{})
	testutil.Equal(t, chatwire.ParseLooseStringAny(envelope["code"]), upstreamRejectionCode)
	got := chatwire.ParseLooseStringAny(envelope["message"])
	testutil.Falsef(t, !strings.Contains(got, "rejected the request parameters"), "envelope message = %q", got)
	testutil.Equal(t, chatwire.ParseLooseStringAny(response["status"]), "failed")
}

// A completed response that merely mentions an error field is not a failure and
// must not be rewritten into one.
func TestRedactResponseErrorLeavesCompletedEnvelopeStatus(t *testing.T) {
	event := map[string]interface{}{
		"response": map[string]interface{}{"id": "resp_1", "status": "completed", "error": nil},
	}
	redactResponseError(event)
	response := event["response"].(map[string]interface{})
	testutil.Equal(t, chatwire.ParseLooseStringAny(response["status"]), "completed")
	testutil.True(t, response["error"] == nil, "successful response must retain error: null")
}

func TestRedactResponseErrorPreservesNullErrors(t *testing.T) {
	for _, status := range []string{"queued", "in_progress", "completed", "incomplete"} {
		t.Run(status, func(t *testing.T) {
			response := map[string]interface{}{"object": "response", "status": status, "error": nil}
			if redactResponseError(response) {
				t.Fatal("null error changed a response")
			}
			if value, exists := response["error"]; !exists || value != nil {
				t.Fatalf("error = %#v, exists = %v; want explicit null", value, exists)
			}
			event := map[string]interface{}{"type": "response." + status, "response": response}
			if redactResponseError(event) || response["error"] != nil {
				t.Fatal("nested SSE response acquired a spurious error")
			}
		})
	}
}

// A gateway-synthesized failure caused by a rejection reports the rejection, so
// a client stops retrying instead of seeing a transport failure.
func TestClassifySynthesizedFailureUpgradesRejection(t *testing.T) {
	cases := []struct {
		name        string
		message     string
		err         error
		wantCode    string
		wantMessage string
	}{
		{
			name:        "protocol error carrying a rejection",
			message:     "stream_read_error",
			err:         errString("grok upstream stream error: model not found"),
			wantCode:    upstreamRejectionCode,
			wantMessage: "The upstream rejected the request parameters or model. Check the request and model selection.",
		},
		{
			name:        "transport error stays a failure",
			message:     "stream_read_error",
			err:         errString("read tcp 10.0.0.1:443: connection reset by peer"),
			wantCode:    "stream_read_error",
			wantMessage: "stream_read_error",
		},
		{
			name:        "no protocol error keeps the synthesized code",
			message:     "upstream_terminal_missing",
			wantCode:    "upstream_terminal_missing",
			wantMessage: "upstream_terminal_missing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, message := classifySynthesizedFailure(tc.message, tc.message, tc.err)
			testutil.Equal(t, code, tc.wantCode)
			testutil.Equal(t, message, tc.wantMessage)
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }
