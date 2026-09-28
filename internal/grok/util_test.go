package grok

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/store"
)

func TestParseDataURI(t *testing.T) {
	name, content, mime, err := parseDataURI("data:image/png;base64,QUJD")
	if err != nil {
		t.Fatalf("parseDataURI error: %v", err)
	}
	if name != "file.png" {
		t.Fatalf("name=%q want=file.png", name)
	}
	if content != "QUJD" {
		t.Fatalf("content=%q want=QUJD", content)
	}
	if mime != "image/png" {
		t.Fatalf("mime=%q want=image/png", mime)
	}
}

func TestValidateChatMessages_AcceptsCaseInsensitiveRoleAndType(t *testing.T) {
	messages := []ChatMessage{
		{
			Role: "User",
			Content: []interface{}{
				map[string]interface{}{"type": "Text", "text": "hello"},
				map[string]interface{}{"type": "Image_URL", "image_url": map[string]interface{}{"url": "https://a/b.png"}},
			},
		},
		{
			Role: "ASSISTANT",
			Content: []interface{}{
				map[string]interface{}{"type": "TEXT", "text": "ok"},
			},
		},
	}

	if err := validateChatMessages(messages); err != nil {
		t.Fatalf("validateChatMessages() error = %v", err)
	}
}

func TestParseUpstreamLines_CollectAndSkip(t *testing.T) {
	raw := strings.Join([]string{
		`{"result":{"response":{"token":"hello"}}}`,
		`{"result":{"other":1}}`,
		`{"other":2}`,
		`{"result":{"response":{"token":"world"}}}`,
	}, "")

	got := make([]string, 0, 2)
	err := parseUpstreamLines(strings.NewReader(raw), func(line map[string]interface{}) error {
		token, _ := line["token"].(string)
		got = append(got, token)
		return nil
	})
	if err != nil {
		t.Fatalf("parseUpstreamLines() error: %v", err)
	}
	if len(got) != 2 || got[0] != "hello" || got[1] != "world" {
		t.Fatalf("unexpected tokens: %v", got)
	}
}

func TestParseUpstreamLines_CallbackError(t *testing.T) {
	expectErr := errors.New("stop")
	raw := `{"result":{"response":{"token":"hello"}}}`
	err := parseUpstreamLines(strings.NewReader(raw), func(line map[string]interface{}) error {
		return expectErr
	})
	if !errors.Is(err, expectErr) {
		t.Fatalf("parseUpstreamLines error=%v want=%v", err, expectErr)
	}
}

func TestParseUpstreamLines_InvalidJSON(t *testing.T) {
	raw := `{"result":{"response":{"token":"hello"}}`
	err := parseUpstreamLines(strings.NewReader(raw), func(line map[string]interface{}) error {
		return nil
	})
	if err == nil {
		t.Fatalf("expected invalid json error")
	}
}

func TestParseUpstreamLines_RejectsUpstreamErrorEnvelope(t *testing.T) {
	err := parseUpstreamLines(strings.NewReader(`{"error":{"code":"forbidden","message":"not allowed"}}`), func(map[string]interface{}) error {
		t.Fatal("callback must not run for upstream errors")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("error=%v want upstream error", err)
	}
}

func TestParseUpstreamLines_RejectsUpstreamEventErrorEnvelope(t *testing.T) {
	err := parseUpstreamLines(strings.NewReader(`{"event":{"type":"error","message":"rate limited"}}`), func(map[string]interface{}) error {
		t.Fatal("callback must not run for upstream event errors")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("error=%v want upstream event error", err)
	}
}

func TestApplyQuotaInfo_InfersLiteSubscription(t *testing.T) {
	acc := &store.Account{Subscription: "basic"}
	changed := ApplyQuotaInfo(acc, &RateLimitInfo{
		Limit:        70,
		HasLimit:     true,
		Remaining:    63,
		HasRemaining: true,
		Unit:         "requests",
	})
	if !changed {
		t.Fatal("ApplyQuotaInfo changed=false, want true")
	}
	if acc.Subscription != "lite" {
		t.Fatalf("subscription=%q want lite", acc.Subscription)
	}
	if acc.UsageLimit != 70 || acc.UsageCurrent != 63 {
		t.Fatalf("unexpected quota: limit=%v current=%v", acc.UsageLimit, acc.UsageCurrent)
	}
}

func TestApplyQuotaInfo_InfersBasicFromFreeAutoWindow(t *testing.T) {
	acc := &store.Account{}
	changed := ApplyQuotaInfo(acc, &RateLimitInfo{
		Limit:        7,
		HasLimit:     true,
		Remaining:    7,
		HasRemaining: true,
		Unit:         "requests",
	})
	if !changed {
		t.Fatal("ApplyQuotaInfo changed=false, want true")
	}
	if acc.Subscription != "basic" {
		t.Fatalf("subscription=%q want basic", acc.Subscription)
	}
	if acc.UsageLimit != 7 || acc.UsageCurrent != 7 {
		t.Fatalf("unexpected quota: limit=%v current=%v", acc.UsageLimit, acc.UsageCurrent)
	}
}

func TestParseRateLimitValue_ComplexFormats(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{in: "100;w=3600", want: 100},
		{in: "23/50", want: 23},
		{in: "remaining=42", want: 42},
		{in: "  7.9 requests", want: 7},
	}

	for _, tt := range tests {
		got, ok := parseRateLimitValue(tt.in)
		if !ok {
			t.Fatalf("parseRateLimitValue(%q) not parsed", tt.in)
		}
		if got != tt.want {
			t.Fatalf("parseRateLimitValue(%q)=%d want=%d", tt.in, got, tt.want)
		}
	}
}

func TestParseRateLimitReset_RFC3339(t *testing.T) {
	raw := "2026-03-05T19:00:00Z"
	got := parseRateLimitReset(raw)
	want, _ := time.Parse(time.RFC3339, raw)
	if !got.Equal(want) {
		t.Fatalf("parseRateLimitReset(%q)=%v want=%v", raw, got, want)
	}
}

func TestEncodeJSONBytesDoesNotEscapeHTML(t *testing.T) {
	payload := map[string]interface{}{
		"type": "chunk",
		"data": map[string]interface{}{
			"text": "hello <world>",
			"n":    1,
		},
	}
	got := string(encodeJSONBytes(payload))
	if !strings.Contains(got, "hello <world>") {
		t.Fatalf("got=%q want unescaped html", got)
	}
}

func TestWriteSSEBytesWritesEventFrame(t *testing.T) {
	bytesRec := httptest.NewRecorder()
	writeSSEBytes(bytesRec, "demo", []byte(`{"ok":true}`))

	got := bytesRec.Body.String()
	if !strings.Contains(got, "event: demo\n") || !strings.Contains(got, `data: {"ok":true}`) {
		t.Fatalf("unexpected sse frame: %q", got)
	}
}

func TestWriteSSEBytesPropagatesShortWrite(t *testing.T) {
	writer := grok2apiShortWriter{httptest.NewRecorder()}
	if err := writeSSEBytes(writer, "demo", []byte(`{"ok":true}`)); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error=%v", err)
	}
}

func TestStreamResponseHeadersMatchSSEProxyContract(t *testing.T) {
	recorder := httptest.NewRecorder()
	recorder.Header().Set("Connection", "keep-alive")
	streamResponseHeaders(recorder)
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("content-type=%q", got)
	}
	if recorder.Header().Get("X-Accel-Buffering") != "no" || recorder.Header().Get("Connection") != "" {
		t.Fatalf("headers=%v", recorder.Header())
	}
}

func BenchmarkEncodeJSON_Bytes(b *testing.B) {
	payload := map[string]interface{}{
		"id": "msg_1",
		"choices": []map[string]interface{}{{
			"index": 0,
			"delta": map[string]interface{}{"content": "hello world"},
		}},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = encodeJSONBytes(payload)
	}
}

func BenchmarkWriteSSE_Bytes(b *testing.B) {
	writer := httptest.NewRecorder()
	data := []byte(`{"ok":true}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		writer.Body.Reset()
		writeSSEBytes(writer, "demo", data)
	}
}
