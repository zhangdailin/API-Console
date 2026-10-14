package grok

import (
	"testing"

	"orchids-api/internal/testutil"
)

func TestResponsesImagePartsCarryDefaultDetail(t *testing.T) {
	parts := responsesMessageParts([]any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
	}, false)
	testutil.Equal(t, len(parts), 1)
	part, _ := parts[0].(map[string]any)
	testutil.Equal(t, part["detail"], "auto")
	// An explicit detail is preserved.
	parts = responsesMessageParts([]any{
		map[string]any{"type": "image_url", "detail": "high", "image_url": map[string]any{"url": "https://example.com/a.png"}},
	}, false)
	part, _ = parts[0].(map[string]any)
	testutil.Equal(t, part["detail"], "high")
}
