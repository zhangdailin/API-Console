package responses

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

func TestAgentMessagesPreserveTaskOrderAndOriginalHistory(t *testing.T) {
	const first = "  Inspect login.go.\n\tKeep indentation and Unicode: 权限  "
	const second = "Then check the permission tests."
	for _, kind := range []string{"agent_message", "message", ""} {
		t.Run(kind, func(t *testing.T) {
			item := map[string]any{"type": kind, "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "Message Type: NEW_TASK\r\nTask name: /root/check\r\nSender: /root\r\nPayload:\r\n"},
				map[string]any{"type": "encrypted_content", "encrypted_content": first},
				map[string]any{"type": "input_text", "text": "Follow-up details:"},
				map[string]any{"type": "encrypted_content", "encrypted_content": second},
				map[string]any{"type": "input_image", "image_url": "https://example.test/screenshot.png"},
			}}
			original := []any{item, map[string]any{"type": "function_call_output", "call_id": "call-1", "output": `{"encrypted_content":"tool data"}`}, map[string]any{"type": "reasoning", "encrypted_content": "opaque-proof"}, map[string]any{"type": "compaction", "encrypted_content": "sealed-summary"}}
			before, err := json.Marshal(original)
			testutil.NoError(t, err)
			normalized, err := NormalizeAgentMessages(original)
			testutil.NoError(t, err)
			after, err := json.Marshal(original)
			testutil.NoError(t, err)
			testutil.Equal(t, string(before), string(after))
			parts := normalized.([]any)[0].(map[string]any)["content"].([]any)
			testutil.Equal(t, len(parts), 5)
			for index, want := range map[int]string{1: first, 3: second} {
				part := parts[index].(map[string]any)
				testutil.EqualAny(t, part["type"], "input_text")
				testutil.EqualAny(t, part["text"], want)
				_, encrypted := part["encrypted_content"]
				testutil.False(t, encrypted)
			}
			testutil.True(t, reflect.DeepEqual(parts[4], item["content"].([]any)[4]))
			testutil.True(t, reflect.DeepEqual(normalized.([]any)[1], original[1]))
			testutil.True(t, reflect.DeepEqual(normalized.([]any)[2:], original[2:]))
			again, err := NormalizeAgentMessages(normalized)
			testutil.NoError(t, err)
			testutil.True(t, reflect.DeepEqual(normalized, again))
		})
	}
}

func TestExplicitAgentMessagesRemainUserData(t *testing.T) {
	for _, content := range []any{"Run the tests.", []any{map[string]any{"type": "encrypted_content", "encrypted_content": "Run the tests."}}} {
		normalized, err := NormalizeAgentMessages([]any{map[string]any{"type": "agent_message", "role": "system", "author": "/root", "recipient": "/root/tests", "id": "amsg_test", "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "private"}, "content": content}})
		testutil.NoError(t, err)
		item := normalized.([]any)[0].(map[string]any)
		testutil.EqualAny(t, item["type"], "message")
		testutil.EqualAny(t, item["role"], "user")
		for _, field := range []string{"id", "author", "recipient", "internal_chat_message_metadata_passthrough"} {
			_, exists := item[field]
			testutil.False(t, exists)
		}
	}
}

func TestAgentNormalizerLeavesOpaqueReasoningCompactionAndOtherMessages(t *testing.T) {
	for _, role := range []string{"user", "assistant", "developer", "system"} {
		for _, envelope := range []string{"Payload:\n", "Message Type: NEW_TASK\n", "Documentation mentions Message Type: NEW_TASK and Payload:", "Payload:\nMessage Type: NEW_TASK\n"} {
			original := []any{
				map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": "input_text", "text": envelope}, map[string]any{"type": "encrypted_content", "encrypted_content": "opaque-proof"}}},
				map[string]any{"type": "reasoning", "encrypted_content": "gAAAA-reasoning-proof", "summary": []any{}},
				map[string]any{"type": "compaction", "encrypted_content": "g2a_compact_v1.sealed-proof"},
			}
			normalized, err := NormalizeAgentMessages(original)
			testutil.NoError(t, err)
			testutil.True(t, reflect.DeepEqual(normalized, original))
			_, err = InputToMessages([]any{original[0]})
			testutil.CheckError(t, err)
			testutil.MustNotContain(t, err.Error(), "opaque-proof")
		}
	}
	// Even a full task envelope cannot turn an assistant's reasoning into user text.
	original := []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "input_text", "text": "Message Type: NEW_TASK\nPayload:\n"}, map[string]any{"type": "encrypted_content", "encrypted_content": "opaque-proof"}}}}
	normalized, err := NormalizeAgentMessages(original)
	testutil.NoError(t, err)
	testutil.True(t, reflect.DeepEqual(normalized, original))
}

func TestMalformedAgentContentFailsWithoutEchoingPayload(t *testing.T) {
	for _, content := range []any{nil, "", []any{}, []any{nil}, []any{map[string]any{"type": "encrypted_content", "encrypted_content": map[string]any{"secret": "do-not-echo"}}}, []any{map[string]any{"type": "encrypted_content", "encrypted_content": " "}}, []any{map[string]any{"type": "unknown", "text": "do-not-echo"}}} {
		_, err := NormalizeAgentMessages([]any{map[string]any{"type": "agent_message", "content": content}})
		testutil.CheckError(t, err)
		testutil.MustNotContain(t, err.Error(), "do-not-echo")
		testutil.True(t, strings.Contains(err.Error(), "input[0]"))
	}
}
