package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/audit"
	"orchids-api/internal/testutil"
	"testing"
)

func TestFlowTrackerTracksAdmittedStreamAndCompletion(t *testing.T) {
	tracker := NewFlowTracker()
	events, unsubscribe := tracker.Subscribe()
	defer unsubscribe()
	entered, finish, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := tracker.Track(func(w http.ResponseWriter, r *http.Request) {
		// The real writer must keep its streaming capabilities.
		if _, ok := w.(http.Flusher); !ok {
			t.Error("lost streaming writer")
		}
		close(entered)
		<-finish
	})
	go func() {
		defer close(done)
		handler(httptest.NewRecorder(), httptest.NewRequest("POST", "/cline/v1/responses", nil))
	}()
	<-entered
	<-events
	snapshot := tracker.Snapshot("cline")
	if len(snapshot.Routes) != 3 {
		t.Fatalf("scope: %+v", snapshot)
	}
	route := snapshot.Routes[1]
	if route.Active != 1 || route.Started != 1 || route.Completed != 0 {
		t.Fatalf("start: %+v", route)
	}
	close(finish)
	<-done
	<-events
	route = tracker.Snapshot("cline").Routes[1]
	if route.Active != 0 || route.Started != 1 || route.Completed != 1 {
		t.Fatalf("finish: %+v", route)
	}
}

func TestFlowTrackerIgnoresResourcesAndReleasesAfterPanic(t *testing.T) {
	tracker := NewFlowTracker()
	for _, target := range []string{"/grok/v1/models", "/grok/v1/messages/count_tokens", "/grok/v1/responses/id/cancel"} {
		tracker.Track(func(http.ResponseWriter, *http.Request) {})(httptest.NewRecorder(), httptest.NewRequest("POST", target, nil))
	}
	func() {
		defer func() { recover() }()
		tracker.Track(func(http.ResponseWriter, *http.Request) { panic("handler panic") })(httptest.NewRecorder(), httptest.NewRequest("POST", "/grok/messages", nil))
	}()
	for _, row := range tracker.Snapshot("").Routes {
		if row.Channel == "grok" && row.Protocol == "messages" {
			if row.Active != 0 || row.Started != 1 || row.Completed != 1 {
				t.Fatalf("panic leaked activity: %+v", row)
			}
		} else if row.Started != 0 {
			t.Fatalf("resource counted as inference: %+v", row)
		}
	}
}

func TestFlowRatesUseObservedUsageWithoutDoubleCounting(t *testing.T) {
	tracker := NewFlowTracker()
	box := &requestObservation{}
	req := httptest.NewRequest("POST", "/cline/v1/responses", nil)
	req = req.WithContext(context.WithValue(req.Context(), requestObservationKey{}, box))
	tracker.Track(func(w http.ResponseWriter, r *http.Request) {
		logger := ObserveAuditLogger(nil)
		logger.Log(r.Context(), audit.Event{Action: "cline_upstream_attempt", Status: "ok", UsageSource: audit.UsageSourceUpstream, InputTokens: 100, OutputTokens: 50})
		during := tracker.Snapshot("cline").Routes[1]
		if !during.UsageAvailable || during.QPS != 0.2 || during.TPS != 30 {
			t.Fatalf("live usage: %+v", during)
		}
		logger.Log(r.Context(), audit.Event{Action: "chat_request", Status: "ok", UsageSource: audit.UsageSourceUpstream, InputTokens: 100, OutputTokens: 50})
	})(httptest.NewRecorder(), req)
	route := tracker.Snapshot("cline").Routes[1]
	if route.InputTokens != 100 || route.OutputTokens != 50 || route.TPS != 30 {
		t.Fatalf("repeated final usage was counted twice: %+v", route)
	}
	tracker.mu.Lock()
	for i := range tracker.routes["cline:responses"].buckets {
		tracker.routes["cline:responses"].buckets[i].second -= 6
	}
	tracker.mu.Unlock()
	route = tracker.Snapshot("cline").Routes[1]
	if route.QPS != 0 || route.TPS != 0 || !route.UsageAvailable {
		t.Fatalf("idle rates did not decay: %+v", route)
	}
}

func TestFlowCountsOnlyUpstreamReportedUsage(t *testing.T) {
	for _, source := range []audit.UsageSource{audit.UsageSourceEstimated, audit.UsageSourceNone, audit.UsageSourceUpstream} {
		t.Run(string(source), func(t *testing.T) {
			tracker := NewFlowTracker()
			box := &requestObservation{}
			ctx := context.WithValue(context.Background(), requestObservationKey{}, box)
			handler := tracker.Track(func(w http.ResponseWriter, r *http.Request) {
				ObserveAuditLogger(nil).Log(r.Context(), audit.Event{Action: "grok_request", UsageSource: source, InputTokens: 100, OutputTokens: 50})
			})
			handler(httptest.NewRecorder(), httptest.NewRequest("POST", "/grok/v1/messages", nil).WithContext(ctx))
			snapshot := tracker.Snapshot("grok")
			for _, route := range snapshot.Routes {
				if route.Channel != "grok" || route.Protocol != "messages" {
					continue
				}
				if source == audit.UsageSourceUpstream {
					testutil.True(t, route.UsageAvailable)
					testutil.Equal(t, route.TPS, float64(30))
				} else {
					testutil.False(t, route.UsageAvailable)
					testutil.Equal(t, route.TPS, float64(0))
				}
			}
		})
	}
}
