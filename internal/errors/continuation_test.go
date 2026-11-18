package errors

import (
	"net/http"
	"strings"
	"testing"
)

func TestWarpOwnedContinuationConflictIsNotRetryable(t *testing.T) {
	for _, text := range []string{
		"status=409: Warp tool continuation expired or belongs to a different owner, account or model; no upstream call was made",
		"status=409: Warp tool results were already consumed or the paid-turn budget is exhausted; no upstream call was made",
	} {
		class := ClassifyUpstreamError(text)
		if class.Category != "continuation_conflict" || class.Retryable || class.SwitchAccount {
			t.Fatal("continuation conflict can replay or switch")
		}
		if StatusForCategory(class.Category) != http.StatusConflict || !strings.Contains(PublicMessage(text), "original conversation") {
			t.Fatal("continuation status or guidance lost")
		}
		if ClassifyAccountStatus(text) != "" {
			t.Fatal("continuation conflict poisons account")
		}
	}
	if ClassifyUpstreamError("status=409: unrelated upstream conflict").Category == "continuation_conflict" {
		t.Fatal("unrelated upstream conflict changed")
	}
}
