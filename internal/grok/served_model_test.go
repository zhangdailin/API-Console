package grok

import (
	"testing"

	"orchids-api/internal/testutil"
)

func TestResponsesServedModelReadsResponseObjectAndEvents(t *testing.T) {
	testutil.Equal(t, responsesServedModel(map[string]any{"object": "response", "model": "grok-4.7-0401"}), "grok-4.7-0401")
	testutil.Equal(t, responsesServedModel(map[string]any{"type": "response.completed", "response": map[string]any{"model": "grok-4.7-0401"}}), "grok-4.7-0401")
	testutil.Equal(t, responsesServedModel(map[string]any{"type": "response.output_text.delta", "delta": "x"}), "")
}
