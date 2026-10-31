package handler

import (
	"testing"
	"time"
)

func TestQueueTypeFromRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{"plain", `{"queueType":"p4"}`, "p4"},
		{"escaped", `{\"queueType\":\"p3\"}`, "p3"},
		{"absent", `{"code":"10605"}`, ""},
		{"quoted body", `body: "{\"isQueued\":true,\"queueType\":\"p4\",\"retryAfterSeconds\":30}"`, "p4"},
	} {
		if got := queueTypeFromRefusal(tc.text); got != tc.want {
			t.Fatalf("%s: queueTypeFromRefusal() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestQueueRefusalDetails(t *testing.T) {
	// A category that is not a queue never carries queue fields.
	if got := queueRefusalDetails("rate_limit", `{"queueType":"p4"}`, time.Second); got != nil {
		t.Fatalf("non-queue category produced details: %v", got)
	}

	got := queueRefusalDetails("upstream_queue", `{"queueType":"p4"}`, 30*time.Second)
	if got["queue_type"] != "p4" {
		t.Fatalf("queue_type = %v, want p4", got["queue_type"])
	}
	if got["retry_after_seconds"] != 30 {
		t.Fatalf("retry_after_seconds = %v, want 30", got["retry_after_seconds"])
	}
	if got["model_switch_recommended"] != true {
		t.Fatalf("model_switch_recommended = %v, want true", got["model_switch_recommended"])
	}

	// A refusal that does not name a queue class publishes no queue_type rather
	// than an empty one.
	noType := queueRefusalDetails("upstream_queue", `the upstream is busy`, 5*time.Second)
	if _, present := noType["queue_type"]; present {
		t.Fatalf("absent queue class was published: %v", noType)
	}
}
