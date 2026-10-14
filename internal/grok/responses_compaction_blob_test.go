package grok

import (
	"orchids-api/internal/responses"
	"strings"
	"testing"
	"unicode/utf8"

	"encoding/json"

	"orchids-api/internal/secureblob"
	"orchids-api/internal/testutil"
)

func TestGatewayCompactionCodecRoundTripAndRejections(t *testing.T) {
	codec := responses.NewCompactionCodec(testCompactionCipher(t))
	blob, err := codec.Encode("session-a", "Summary:\nkept text")
	testutil.NoError(t, err, "encode: %v")
	testutil.Falsef(t, !strings.HasPrefix(blob, responses.CompactionPrefix), "blob=%q missing prefix", blob)

	summary, owned, drifted, err := codec.Decode("session-a", blob)
	testutil.Falsef(t, err != nil || !owned || drifted || summary != "Summary:\nkept text", "decode=%q owned=%v drifted=%v err=%v", summary, owned, drifted, err)
	// Session is advisory: a drifted key still decrypts, and the caller learns it.
	_, owned, drifted, err = codec.Decode("session-b", blob)
	testutil.Falsef(t, err != nil || !owned || !drifted, "drifted decode owned=%v drifted=%v err=%v", owned, drifted, err)
	// A foreign blob is not ours and must pass through untouched.
	summary, owned, _, err = codec.Decode("session-a", "upstream-opaque-blob")
	testutil.Falsef(t, err != nil || owned || summary != "", "foreign blob summary=%q owned=%v err=%v", summary, owned, err)
	// A prefixed blob that cannot be opened is an error, never an empty summary.
	_, owned, _, err = codec.Decode("session-a", responses.CompactionPrefix+"not-base64!!")
	testutil.Falsef(t, err == nil || !owned, "tampered blob owned=%v err=%v", owned, err)
	// Another instance (different key) must not be able to read ours.
	otherCodec := responses.NewCompactionCodec(mustCipher(t, "fedcba9876543210fedcba9876543210"))
	_, _, _, err = otherCodec.Decode("session-a", blob)
	testutil.Error(t, err)
	// Size limits are enforced on both ends.
	_, err = codec.Encode("s", "")
	testutil.Error(t, err)
	_, err = codec.Encode("s", strings.Repeat("x", responses.MaxCompactionSummary+1))
	testutil.Error(t, err)
}

func mustCipher(t *testing.T, key string) *secureblob.Cipher {
	t.Helper()
	cipher, err := secureblob.NewCipher([]byte(key))
	testutil.NoError(t, err, "secureblob.NewCipher: %v")
	return cipher
}

func TestExpandGatewayCompactionHistory(t *testing.T) {
	codec := responses.NewCompactionCodec(testCompactionCipher(t))
	blob, err := codec.Encode("session-a", "Summary:\nreplayed")
	testutil.NoError(t, err, "encode: %v")

	payload := map[string]any{"input": []any{
		map[string]any{"type": "message", "role": "user", "content": "hello"},
		map[string]any{"id": "cmp_1", "type": "compaction", "encrypted_content": blob},
		map[string]any{"id": "cmp_2", "type": "compaction", "encrypted_content": "upstream-opaque"},
	}}
	drifted, err := responses.ExpandCompactionHistory(payload, codec, "session-a")
	testutil.Equal(t, err, nil)
	testutil.Equal(t, drifted, 0)
	items := payload["input"].([]any)
	expanded := items[1].(map[string]any)
	testutil.Equal(t, expanded["type"], "message")
	testutil.Equal(t, expanded["role"], "user")
	parts := expanded["content"].([]any)
	testutil.Equal(t, parts[0].(map[string]any)["text"], "Summary:\nreplayed")
	// The upstream's own blob is handed to the upstream unchanged.
	foreign := items[2].(map[string]any)
	testutil.Equal(t, foreign["type"], "compaction")
	testutil.Equal(t, foreign["encrypted_content"], "upstream-opaque")

	// An undecodable gateway blob names the item that has to be dropped.
	bad := map[string]any{"input": []any{
		map[string]any{"type": "compaction", "encrypted_content": responses.CompactionPrefix + "broken"},
	}}
	_, err = responses.ExpandCompactionHistory(bad, codec, "session-a")
	testutil.False(t, err == nil, "expected an error for an undecodable gateway blob")
	var blobErr *responses.CompactionBlobError
	testutil.Falsef(t, !asError(err, &blobErr) || blobErr.Param() != "input[0].encrypted_content", "err=%v param=%q", err, compactionErrorParam(err))
}

func TestCleanGatewayCompactionSummary(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "analysis block ahead of the summary is dropped",
			raw:  "<analysis>thinking out loud</analysis>\n<summary>1. Request: keep going</summary>",
			want: "Summary:\n1. Request: keep going",
		},
		{
			// The upstream cleaner deletes an <analysis> block whenever it
			// precedes <summary>, even with prose in front of it. Ported verbatim,
			// so the expectation records that behaviour rather than an ideal.
			name: "analysis block ahead of the summary is dropped even with leading prose",
			raw:  "The user said <analysis> earlier.\n<summary>1. Request: keep going</summary>",
			want: "The user said Summary:\n1. Request: keep going",
		},
		{
			name: "a stray opening tag after the summary is defused",
			raw:  "Summary:\n1. Request: keep going <analysis> not a block",
			want: "Summary:\n1. Request: keep going <\u200banalysis> not a block",
		},
		{
			name: "orphan closing tag after markdown scratchpad is stripped",
			raw:  "<summary># scratch\n</analysis>1. Request: keep going</summary>",
			want: "Summary:\n1. Request: keep going",
		},
		{
			name: "numbered summary keeps its own analysis mention",
			raw:  "<summary>1. Request: it mentioned </analysis> verbatim</summary>",
			want: "Summary:\n1. Request: it mentioned <\u200b/analysis> verbatim",
		},
		{
			name: "blank runs collapse",
			raw:  "Summary:\n\n\n1. Request: keep going",
			want: "Summary:\n\n1. Request: keep going",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutil.Equal(t, responses.CompactionCleanSummary(tc.raw), tc.want)
		})
	}
}

func TestDegenerateGatewayCompactionSummary(t *testing.T) {
	testutil.False(t, !responses.IsDegenerateCompactionSummary("too short"), "a short summary must count as degenerate")
	// Tags and blank padding must not be able to fake length.
	testutil.False(t, !responses.IsDegenerateCompactionSummary("<summary>"+strings.Repeat("\n", 600)+"short</summary>"), "padding counted towards the summary length")
	testutil.False(t, responses.IsDegenerateCompactionSummary(compactionTestSummary), "a real summary was rejected")
	testutil.Falsef(t, utf8.RuneCountInString(responses.CompactionCleanSummary(compactionTestSummary)) < responses.MinCompactionRunes, "fixture summary is too short: %d runes", utf8.RuneCountInString(compactionTestSummary))
}

func TestPrepareGatewayCompactionSample(t *testing.T) {
	payload := map[string]any{
		"model": "grok-4.5", "stream": false, "instructions": "be terse",
		"temperature": 0.2, "store": true, "previous_response_id": "resp_1",
		"max_output_tokens": 4096, "text": map[string]any{"format": "json"},
		"input":       []any{map[string]any{"type": "message", "role": "user", "content": "hello"}},
		"tools":       []any{map[string]any{"type": "function", "name": "weather"}},
		"tool_choice": "none",
	}
	sample := responses.PrepareCompactionSample(payload)

	testutil.Equal(t, sample["stream"], true)
	testutil.Equal(t, sample["store"], false)
	if sample["instructions"] != nil {
		t.Fatalf("instructions=%v want nil", sample["instructions"])
	}
	testutil.Equal(t, sample["temperature"], 1.0)
	testutil.Equal(t, sample["tool_choice"], "auto")
	reasoning, _ := sample["reasoning"].(map[string]any)
	testutil.Equal(t, reasoning["summary"], "concise")
	for _, dropped := range []string{"previous_response_id", "text", "max_output_tokens", "max_completion_tokens"} {
		_, present := sample[dropped]
		testutil.Falsef(t, present, "%s survived sample preparation", dropped)
	}
	items := sample["input"].([]any)
	last := items[len(items)-1].(map[string]any)
	testutil.EqualAny(t, last["content"], responses.CompactionPrompt)
	// The caller's payload must not be mutated: it is still the request body.
	_, present := payload["reasoning"]
	testutil.False(t, present, "prepareGatewayCompactionSample mutated its input")
	testutil.False(t, len(payload["input"].([]any)) != 1, "prepareGatewayCompactionSample appended to the caller's input")
	// Without tools, a stale tool_choice is removed rather than forwarded.
	noTools := responses.PrepareCompactionSample(map[string]any{"model": "grok-4.5", "input": []any{}, "tool_choice": "none"})
	_, present = noTools["tool_choice"]
	testutil.False(t, present, "tool_choice survived without tools")
}

func TestParseGatewayCompactionStream(t *testing.T) {
	completed := map[string]any{
		"id": "resp_1", "output": []any{map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": compactionTestSummary}},
		}},
	}
	completedJSON, _ := json.Marshal(map[string]any{"type": "response.completed", "response": completed})
	stream := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"event: response.completed\ndata: " + string(completedJSON) + "\n\n"
	sample, err := responses.ParseCompactionStream([]byte(stream))
	testutil.NoError(t, err, "parse: %v")
	testutil.Equal(t, sample.Summary, strings.TrimSpace(compactionTestSummary))
	testutil.Equal(t, sample.Response["id"], "resp_1")

	// No completed event at all is a retryable failure, not an empty summary.
	_, err = responses.ParseCompactionStream([]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
	testutil.Error(t, err)

	// A failed response carries the upstream reason and its retryability.
	failed := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"invalid_request_error\",\"message\":\"bad input\"}}}\n\n"
	_, err = responses.ParseCompactionStream([]byte(failed))
	testutil.Falsef(t, err == nil || responses.CompactionErrorIsTransient(err), "err=%v transient=%v", err, responses.CompactionErrorIsTransient(err))
	transient := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"try later\"}}}\n\n"
	_, err = responses.ParseCompactionStream([]byte(transient))
	testutil.Falsef(t, err == nil || !responses.CompactionErrorIsTransient(err), "err=%v transient=%v", err, responses.CompactionErrorIsTransient(err))

	// A summary that only arrives as streamed output items is still usable.
	streamedOnly := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"" +
		strings.ReplaceAll(compactionTestSummary, "\n", "\\n") + "\"}]}}\n\n" +
		"event: response.completed\ndata: " + string(completedJSON) + "\n\n"
	sample, err = responses.ParseCompactionStream([]byte(streamedOnly))
	testutil.Falsef(t, err != nil || sample.Summary == "", "summary=%q err=%v", sample.Summary, err)
}
