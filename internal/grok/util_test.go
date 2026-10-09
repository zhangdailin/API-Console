package grok

import (
	"orchids-api/internal/chatwire"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestValidateChatMessages_AcceptsCaseInsensitiveRoleAndType(t *testing.T) {
	messages := []chatwire.Message{
		{
			Role: "User",
			Content: []interface{}{
				map[string]interface{}{"type": "Text", "text": "hello"},
				map[string]interface{}{"type": "Image_URL", "image_url": map[string]interface{}{"url": "https://a/b.png"}},
			},
		},
		{
			Role:    "ASSISTANT",
			Content: []interface{}{map[string]interface{}{"type": "TEXT", "text": "ok"}},
		},
	}

	testutil.NoError(t, chatwire.ValidateMessages(messages), "chatwire.ValidateMessages() error = %v")
}

func TestApplyQuotaInfo_InfersLiteSubscription(t *testing.T) {
	acc := &store.Account{Subscription: "basic"}
	changed := ApplyQuotaInfo(acc, &chatwire.RateLimitInfo{
		Limit:        70,
		HasLimit:     true,
		Remaining:    63,
		HasRemaining: true,
		Unit:         "requests",
	})
	testutil.True(t, changed, "ApplyQuotaInfo changed=false, want true")
	testutil.Equal(t, acc.Subscription, "lite")
	testutil.Equal(t, acc.UsageLimit, 70)
	testutil.Equal(t, acc.UsageCurrent, 63)
}

func TestApplyQuotaInfo_InfersBasicFromFreeAutoWindow(t *testing.T) {
	acc := &store.Account{}
	changed := ApplyQuotaInfo(acc, &chatwire.RateLimitInfo{
		Limit:        7,
		HasLimit:     true,
		Remaining:    7,
		HasRemaining: true,
		Unit:         "requests",
	})
	testutil.True(t, changed, "ApplyQuotaInfo changed=false, want true")
	testutil.Equal(t, acc.Subscription, "basic")
	testutil.Equal(t, acc.UsageLimit, 7)
	testutil.Equal(t, acc.UsageCurrent, 7)
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
		testutil.True(t, ok, "parseRateLimitValue(%q) not parsed")
		testutil.Equal(t, got, tt.want)
	}
}

func TestParseRateLimitReset_RFC3339(t *testing.T) {
	raw := "2026-03-05T19:00:00Z"
	got := parseRateLimitReset(raw)
	want, _ := time.Parse(time.RFC3339, raw)
	testutil.Falsef(t, !got.Equal(want), "parseRateLimitReset(%q)=%v want=%v", raw, got, want)
}
