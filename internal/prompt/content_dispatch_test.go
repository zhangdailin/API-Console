package prompt

import (
	"encoding/json"
	"orchids-api/internal/testutil"
	"strconv"
	"strings"
	"testing"
)

// MessageContent.UnmarshalJSON dispatches on the first byte. The shapes below are
// the ones the previous trial-unmarshal accepted or rejected, so the dispatch has
// to agree with it on every one of them — a wrong armed result would silently
// change how a message is read downstream (IsString decides string vs blocks).
func TestMessageContentUnmarshalDispatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		in       string
		wantText string
		wantBlks int
		wantErr  bool
	}{
		{name: "string", in: `"hello"`, wantText: "hello"},
		{name: "empty string", in: `""`, wantText: ""},
		{name: "string that reads null", in: `"null"`, wantText: "null"},
		{name: "null", in: `null`, wantText: ""},
		{name: "array one text block", in: `[{"type":"text","text":"a"}]`, wantBlks: 1},
		{name: "array tool_result", in: `[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]`, wantBlks: 1},
		{name: "empty array", in: `[]`, wantBlks: 0},
		{name: "leading whitespace string", in: "   \"padded\"", wantText: "padded"},
		{name: "leading whitespace array", in: "\n\t[{\"type\":\"text\",\"text\":\"b\"}]", wantBlks: 1},
		{name: "number is rejected", in: `42`, wantErr: true},
		{name: "object is rejected", in: `{"text":"x"}`, wantErr: true},
		{name: "whitespace only is rejected", in: "   ", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mc MessageContent
			err := json.Unmarshal([]byte(tc.in), &mc)
			if tc.wantErr {
				testutil.Falsef(t, err == nil, "expected an error for %s, got Text=%q Blocks=%d", tc.in, mc.Text, len(mc.Blocks))
				return
			}
			testutil.Falsef(t, err != nil, "unexpected error for %s: %v", tc.in, err)
			testutil.Equal(t, mc.Text, tc.wantText)
			testutil.Equal(t, len(mc.Blocks), tc.wantBlks)
			// Exactly one representation is armed: a string feed leaves Blocks nil,
			// a block feed clears Text.
			testutil.False(t, tc.wantText != "" && !mc.IsString(), "string content should report IsString")
			testutil.False(t, tc.wantBlks > 0 && mc.IsString(), "block content should not report IsString")
		})
	}
}

// A whole message array through the custom decoder, so the marshal path is
// covered too.
func TestMessageContentRoundTrip(t *testing.T) {
	t.Parallel()

	in := []byte(`{"role":"user","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/tmp/a"}}]}`)
	var m Message
	testutil.NoError(t, json.Unmarshal(in, &m))
	testutil.Equal(t, len(m.Content.Blocks), 2)
	testutil.Equal(t, m.Content.Blocks[0].Text, "hi")
	raw, err := json.Marshal(m)
	testutil.NoError(t, err)
	testutil.MustContainAll(t, string(raw), `"text":"hi"`, `"name":"Read"`)
}

// realisticConversation builds the shape a coding harness sends: every message
// carries array content, which is the case the dispatch exists for.
func realisticConversation(messages, blockChars int) []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"claude-sonnet-4-5","messages":[`)
	for i := range messages {
		if i > 0 {
			sb.WriteByte(',')
		}
		text := strings.Repeat("x", blockChars)
		sb.WriteString(`{"role":"user","content":[{"type":"text","text":`)
		raw, _ := json.Marshal(text)
		sb.Write(raw)
		sb.WriteString(`},{"type":"tool_result","tool_use_id":"toolu_`)
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString(`","content":[{"type":"text","text":"ok"}]}]}`)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

func BenchmarkMessageContentArrayUnmarshal(b *testing.B) {
	payload := realisticConversation(40, 800)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var v struct {
			Messages []Message `json:"messages"`
		}
		if err := json.Unmarshal(payload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMessageContentStringUnmarshal(b *testing.B) {
	payload := []byte(`{"role":"user","content":"` + strings.Repeat("y", 800) + `"}`)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var v Message
		if err := json.Unmarshal(payload, &v); err != nil {
			b.Fatal(err)
		}
	}
}
