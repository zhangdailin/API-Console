package doubao

import (
	"errors"
	"strings"
	"testing"
)

func TestChatStreamRecognizesDoubaoReplyEnd(t *testing.T) {
	const (
		prefix = "event: SSE_ACK\ndata: {}\n\n" +
			"event: CHUNK_DELTA\ndata: {\"text\":\"Hello\"}\n\n" +
			"event: CHUNK_DELTA\ndata: {\"text\":\" there\"}\n\n" +
			"event: STREAM_CHUNK\ndata: {\"message_id\":\"assistant\",\"patch_op\":[{\"patch_object\":1,\"patch_value\":{\"content_block\":[{\"block_type\":10000,\"patch_type\":1,\"is_finish\":true}]}}]}\n\n"
		snapshot    = "event: STREAM_MSG_NOTIFY\ndata: {\"content\":{\"content_block\":[{\"content\":{\"text_block\":{\"text\":\"Hello there\"}}}]}}\n\n"
		messageEnd  = "event: SSE_REPLY_END\ndata: {\"end_type\":1,\"msg_finish_attr\":{\"msgid\":\"assistant\"}}\n\n"
		answerEnd   = "event: SSE_REPLY_END\ndata: {\"end_type\":2,\"answer_finish_attr\":{\"has_suggest\":true}}\n\n"
		suggestions = "event: STREAM_CHUNK\ndata: {\"patch_op\":[{\"patch_object\":50,\"patch_type\":1}]}\n\n"
		streamEnd   = "event: SSE_REPLY_END\ndata: {\"end_type\":3}\n\n"
	)
	for _, tc := range []struct {
		name, body string
		output     string
		err        error
	}{
		{name: "captured_end_sequence", body: prefix + messageEnd + answerEnd + suggestions + streamEnd, output: "Hello there"},
		{name: "final_marker_only", body: prefix + streamEnd, output: "Hello there"},
		{name: "legacy_done", body: prefix + "data: [DONE]\n\n", output: "Hello there"},
		{name: "missing_final_marker", body: prefix + messageEnd + answerEnd, output: "Hello there", err: errIncompleteChatStream},
		{name: "snapshot_does_not_override_missing_final_marker", body: prefix + snapshot + messageEnd + answerEnd, output: "Hello there", err: errIncompleteChatStream},
		{name: "truncated_final_frame", body: prefix + messageEnd + answerEnd + strings.TrimSuffix(streamEnd, "\n"), output: "Hello there", err: errIncompleteChatStream},
		{name: "malformed_final_frame", body: prefix + messageEnd + "event: SSE_REPLY_END\ndata: {\"end_type\":3,}\n\n", output: "Hello there", err: errIncompleteChatStream},
		{name: "non_integer_end_type", body: prefix + "event: SSE_REPLY_END\ndata: {\"end_type\":3.5}\n\n", output: "Hello there", err: errIncompleteChatStream},
		{name: "no_readable_answer", body: messageEnd + answerEnd + streamEnd, err: errIncompleteChatStream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var emitted strings.Builder
			answer, err := consumeChatStream(strings.NewReader(tc.body), func(s string) { emitted.WriteString(s) }, func(string) {})
			if !errors.Is(err, tc.err) {
				t.Fatalf("error = %v, want %v", err, tc.err)
			}
			if got := emitted.String(); got != tc.output {
				t.Fatalf("emitted %q, want %q", got, tc.output)
			}
			if tc.err == nil && answer != tc.output || tc.err != nil && answer != "" {
				t.Fatalf("unexpected returned answer %q", answer)
			}
		})
	}

	t.Run("error_after_final_marker_takes_precedence", func(t *testing.T) {
		body := prefix + streamEnd + "event: STREAM_ERROR\ndata: [DONE]\n\n"
		_, err := consumeChatStream(strings.NewReader(body), func(string) {}, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "malformed Doubao stream error") {
			t.Fatalf("wanted the actual stream error, got %v", err)
		}
	})
}

func TestChatStreamSeparatesAnswerAfterRepeatedThinkingBlock(t *testing.T) {
	const (
		thinking = "event: STREAM_CHUNK\ndata: {\"content_block\":[{\"block_type\":10040,\"block_id\":\"think\",\"parent_id\":\"\",\"patch_type\":2}]}\n\n"
		nested   = "event: STREAM_CHUNK\ndata: {\"content_block\":[{\"block_type\":10000,\"block_id\":\"plan\",\"parent_id\":\"think\",\"patch_type\":1,\"content\":{\"text_block\":{\"text\":\"plan\"}}}]}\n\n"
		summary  = "event: STREAM_CHUNK\ndata: {\"content_block\":[{\"block_type\":10000,\"block_id\":\"plan\",\"parent_id\":\"think\",\"patch_type\":1,\"content\":{\"text_block\":{\"summary\":\"plan metadata\"}}}]}\n\n"
		answer   = "event: STREAM_CHUNK\ndata: {\"content_block\":[{\"block_type\":10000,\"block_id\":\"answer\",\"patch_type\":1,\"content\":{\"text_block\":{\"text\":\"Final\"}}}]}\n\n"
		delta    = "event: CHUNK_DELTA\ndata: {\"text\":\" answer\"}\n\n"
		latePlan = "event: STREAM_CHUNK\ndata: {\"content_block\":[{\"block_type\":10000,\"block_id\":\"plan\",\"parent_id\":\"think\",\"patch_type\":2,\"content\":{\"text_block\":{\"text\":\"plan update\"}}}]}\n\n"
		end      = "event: SSE_REPLY_END\ndata: {\"end_type\":3}\n\n"
	)
	for _, tc := range []struct {
		name, stream, text, reasoning string
		err                           error
	}{
		{"captured repeated thinking block", thinking + summary + thinking + summary + thinking + answer + delta + end + end, "Final answer", "", nil},
		{"late thought stays separate", thinking + nested + thinking + answer + latePlan + delta + end, "Final answer", "plan update", nil},
		{"reasoning only is incomplete", thinking + nested + thinking + end, "", "plan", errIncompleteChatStream},
		{"legacy marker without a child", thinking + answer + delta + end, "", "Final answer", errIncompleteChatStream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var text, reasoning strings.Builder
			answer, err := consumeChatStream(strings.NewReader(tc.stream), func(v string) { text.WriteString(v) }, func(v string) { reasoning.WriteString(v) })
			if !errors.Is(err, tc.err) || answer != tc.text || text.String() != tc.text || reasoning.String() != tc.reasoning {
				t.Fatalf("answer = %q, text = %q, reasoning = %q, error = %v", answer, text.String(), reasoning.String(), err)
			}
		})
	}
}
