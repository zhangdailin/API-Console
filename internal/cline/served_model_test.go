package cline

import (
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

func TestConsumeStreamRecordsTheServedModel(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"c1","model":"anthropic/claude-sonnet-4.6","choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"id":"c1","model":"anthropic/claude-sonnet-4.6","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n") + "\n"
	result, err := consumeStreamWithToolNames(strings.NewReader(body), false, nil, nil)
	testutil.NoError(t, err, "consume: %v")
	testutil.Equal(t, result.Model, "anthropic/claude-sonnet-4.6")
}
