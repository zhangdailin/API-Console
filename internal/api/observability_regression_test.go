package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/alerting"
	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
)

func TestJournalDiagnosticLookupMatchesRequest(t *testing.T) {
	s, _ := newTestStore(t, "diagnostic-api:")
	a := &API{store: s}
	a.SetDiagnosticStore(debug.NewDiagnosticStore(s.RedisClient(), s.RedisPrefix()))
	_, capture := debug.WithCapture(context.Background(), "req-1")
	capture.Set("1_http_request.json", `{"model":"test"}`)
	testutil.NoError(t, a.diagnostics.Save(context.Background(), capture.Bundle()))
	seedJournal(t, a, []audit.Event{{Kind: audit.KindRequest, Action: "chat_request", RequestID: "req-1", Status: "success"}, {Kind: audit.KindRequest, Action: "chat_request", RequestID: "other", Status: "success"}})
	list := journalRequest(t, a, "?kind=request")
	for _, raw := range list["data"].([]any) {
		row := raw.(map[string]any)
		event := row["event"].(map[string]any)
		_, present := row["diagnostics"]
		testutil.Equal(t, present, (event["request_id"] == "req-1"))
	}
	for _, tc := range []struct {
		id        string
		code      int
		available bool
	}{{"req-1", 200, true}, {"other", 200, false}, {"", 400, false}} {
		rec := httptest.NewRecorder()
		a.HandleJournalDiagnostics(rec, httptest.NewRequest(http.MethodGet, "/api/journal/diagnostics?request_id="+tc.id, nil))
		testutil.Equal(t, rec.Code, tc.code)
		if tc.code == 200 {
			var payload map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &payload)
			testutil.EqualAny(t, payload["available"], tc.available)
		}
	}
}
func TestRuntimeResourcesIncludeHostMetrics(t *testing.T) {
	rec := httptest.NewRecorder()
	(&API{}).HandleOpsRuntime(rec, httptest.NewRequest("GET", "/api/ops/runtime", nil))
	var payload struct {
		Metrics []struct {
			Label string `json:"label"`
		}
	}
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	names := map[string]bool{}
	for _, m := range payload.Metrics {
		names[m.Label] = true
	}
	for _, name := range []string{"主机 CPU", "主机内存", "进程内存 RSS", "Go 堆内存"} {
		testutil.Falsef(t, !names[name], "missing %s", name)
	}
	total, idle, err := parseCPUTicks("cpu 10 20 30 40 5 6 7 8 999 999\n")
	testutil.Falsef(t, err != nil || total != 126 || idle != 45, "ticks=%d/%d %v", total, idle, err)
	used, mem := parseHostMemory("MemTotal: 1000 kB\nMemAvailable: 250 kB\n")
	testutil.False(t, used != 750*1024 || mem != 1000*1024, "memory units")
}
func TestAlertRulePersistenceFailureKeepsPolicy(t *testing.T) {
	s, server := newTestStore(t, "rule-failure:")
	a := &API{store: s, alertEngine: alerting.NewEngine(alerting.DefaultRules(), nil)}
	before := a.alertEngine.Thresholds()
	next := before
	next.SuccessRateWarning = .8
	raw, _ := json.Marshal(next)
	server.SetError("storage unavailable")
	rec := httptest.NewRecorder()
	a.HandleOpsAlertRules(rec, httptest.NewRequest("PUT", "/api/ops/alerts/rules", strings.NewReader(string(raw))))
	testutil.Equal(t, rec.Code, 500)
	testutil.Equal(t, a.alertEngine.Thresholds(), before)
}
