package debug

import (
	"strings"
	"testing"
)

type countedSSEPayload struct{ calls *int }

func (p countedSSEPayload) MarshalJSON() ([]byte, error) {
	*p.calls = *p.calls + 1
	return []byte(`{"message":"fixture"}`), nil
}

func TestOutputSSEJSONOnlyEncodesWhenRecording(t *testing.T) {
	calls := 0
	payload := countedSSEPayload{calls: &calls}
	var absent *Logger
	for _, logger := range []*Logger{absent, New(false, true), {enabled: true, sseEnabled: false}} {
		logger.LogOutputSSEJSON("response", payload)
		logger.LogOutputSSEBytes("", []byte("[DONE]"))
	}
	if calls != 0 {
		t.Fatalf("inactive loggers encoded %d frames", calls)
	}

	ctx, capture := WithCapture(t.Context(), "fixture")
	logger := NewForContext(ctx, false, false)
	logger.LogOutputSSEJSON("response", payload)
	logger.LogOutputSSEBytes("", []byte("[DONE]"))
	if calls != 1 {
		t.Fatalf("active capture encoded %d frames", calls)
	}
	for _, section := range capture.Bundle().Sections {
		if section.Name == "5_client_sse.jsonl" && strings.Contains(section.Payload, `data: {"message":"fixture"}`) && strings.Contains(section.Payload, "data: [DONE]") {
			return
		}
	}
	t.Fatal("active capture did not retain both frames")
}
