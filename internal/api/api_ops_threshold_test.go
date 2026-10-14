package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/alerting"
	"orchids-api/internal/testutil"
)

// TestSuccessTarget_ComesFromTheEngine pins why the number exists: the page shows
// a success rate with no stated target, so the threshold the alerts fire on is
// published next to it. It must come from the engine, not a literal here, or the
// two would drift apart.
func TestSuccessTarget_ComesFromTheEngine(t *testing.T) {
	engine := alerting.NewEngine(alerting.DefaultRules(), nil)
	target, source := successTarget(engine)
	testutil.Equal(t, target, 0.9)
	testutil.NotEqual(t, source, "")

	custom := alerting.DefaultRules()
	custom.SuccessRateWarning = 0.8
	got, _ := successTarget(alerting.NewEngine(custom, nil))
	testutil.Falsef(t, got != 0.8, "successTarget() with a custom policy = %v, want 0.8", got)
}

// TestSuccessTarget_NilAndZeroValueFallBackToTheShippedPolicy covers the two ways
// the threshold can be missing: no engine wired at all (aggregation disabled,
// tests) and a Rules zero value. Both must yield 0.9 — a returned 0 would make the
// page print "目标 0.0%" and look broken.
func TestSuccessTarget_NilAndZeroValueFallBackToTheShippedPolicy(t *testing.T) {
	got, source := successTarget(nil)
	testutil.Equal(t, got, 0.9)
	testutil.NotEqual(t, source, "")

	zero := alerting.NewEngine(alerting.Rules{}, nil)
	got, _ = successTarget(zero)
	testutil.Falsef(t, got != 0.9, "successTarget() on a zero-value Rules = %v, want 0.9", got)
	testutil.Equal(t, successTargetCritical(zero), 0.5)
	testutil.Equal(t, successTargetCritical(nil), 0.5)
}

// TestSuccessTarget_SourceIsRenderable keeps the provenance string a UI can print
// as-is: short, and free of the characters that would need escaping in JSON.
func TestSuccessTarget_SourceIsRenderable(t *testing.T) {
	_, source := successTarget(nil)
	testutil.Falsef(t, len([]rune(source)) > 40, "success_target_source too long for a label: %q", source)
	for _, forbidden := range []string{"\"", "\\", "\n", "\t", "<", ">"} {
		testutil.MustNotContain(t, source, forbidden)
	}
}

// TestHandleOpsOverview_PublishesTheTargetOnTheUnavailableBranch is the early
// return: when aggregation is off the handler answers and leaves, so the target
// has to be in the payload before that. The page still shows a success rate in
// that state and must be able to say what it is measured against.
func TestHandleOpsOverview_PublishesTheTargetOnTheUnavailableBranch(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&API{}).HandleOpsOverview(recorder, httptest.NewRequest(http.MethodGet, "/api/ops/overview", nil))

	testutil.Equal(t, recorder.Code, http.StatusOK)
	payload := map[string]any{}
	err := json.Unmarshal(recorder.Body.Bytes(), &payload)
	testutil.CheckNoError(t, err)
	if available, _ := payload["available"].(bool); available {
		t.Fatalf("expected the unavailable branch, got available = %v", payload["available"])
	}
	target, ok := payload["success_target"].(float64)
	testutil.True(t, ok, "success_target missing from the unavailable payload: %v")
	testutil.Equal(t, target, 0.9)
	_, ok = payload["success_target_source"].(string)
	testutil.Falsef(t, !ok, "success_target_source missing: %v", payload)
}
