package workbuddy

import (
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

func TestConsumeStreamRecordsTheServedModel(t *testing.T) {
	body := strings.Join([]string{
		`data: {"model":"glm-5.1-0302","choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"model":"glm-5.1-0302","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n") + "\n"
	result, err := consumeStream(strings.NewReader(body), nil)
	testutil.NoError(t, err, "consumeStream: %v")
	testutil.Equal(t, result.Model, "glm-5.1-0302")
}
