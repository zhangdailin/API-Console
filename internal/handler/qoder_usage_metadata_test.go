package handler

import (
	"orchids-api/internal/testutil"
	"testing"
)

func TestQoderNonbillableMetadataSurvives(t *testing.T) {
	h := &streamHandler{}
	h.applyUpstreamUsageTokens(map[string]any{"inputTokens": 27260, "outputTokens": 48, "billable": false, "cacheable_tokens": 27254, "credits": 0.4885985714285714})
	h.applyUpstreamUsageTokens(map[string]any{"firstTokenDuration": int64(1556), "totalDuration": int64(2577), "serverDuration": int64(89)})
	testutil.Falsef(t, h.inputTokens != 27260 || h.outputTokens != 48 || h.cacheWriteTokens != 0 || h.usageMetadata["billable"] != false || h.usageMetadata["totalDuration"] != int64(2577), "lost or misclassified usage: %#v", h.usageMetadata)
}
