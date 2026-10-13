package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/audit"
	"orchids-api/internal/testutil"
	"testing"
	"time"
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
	if route.InputTPS != 20 || route.OutputTPS != 10 {
		t.Fatalf("input/output rates: %+v", route)
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

func TestFlowMetricsSeparateGenerationVisibleAndNonStream(t *testing.T) {
	tracker := NewFlowTracker()
	box := &requestObservation{attempts: 3, switches: 1, retryWaitMS: 120, queueWaitMS: 80,
		httpPhases:      map[string]int64{"dns_ms": 10, "response_headers_ms": 90},
		httpPhaseCounts: map[string]int64{"dns_ms": 1, "response_headers_ms": 3}}
	ctx := context.WithValue(context.Background(), requestObservationKey{}, box)
	tracker.Track(func(w http.ResponseWriter, r *http.Request) {
		traced := flowTracedWriter(w)
		traced.startedAt = time.Now().Add(-time.Second)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(": keepalive\n\ndata: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		before := tracker.Snapshot("cline").Nodes["cline"]
		if before.Waiting != 1 || before.Responding != 0 {
			t.Fatalf("control frame counted as output: %+v", before)
		}
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n"))
		if progress := tracker.Snapshot("cline").Nodes["responses"]; progress.Waiting != 0 || progress.Responding != 1 || progress.LongestActiveMS < 0 {
			t.Fatalf("missing progress: %+v", progress)
		}
		// Explicit timestamps avoid timing-dependent assertions.
		traced.contentWriteAt = traced.startedAt.Add(100 * time.Millisecond)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n"))
		traced.visibleWriteAt = traced.startedAt.Add(400 * time.Millisecond)
	})(httptest.NewRecorder(), httptest.NewRequest("POST", "/cline/v1/responses", nil).WithContext(ctx))
	stats := tracker.Snapshot("").Nodes["gateway"]
	if stats.FirstGeneration.Samples != 1 || *stats.FirstGeneration.P95MS != 100 || *stats.FirstVisible.P95MS != 400 || stats.BodyTTFB.Samples != 0 {
		t.Fatalf("mixed latency semantics: %+v", stats)
	}
	if stats.Observed != 1 || stats.Retried != 1 || stats.Attempts != 3 || stats.AccountSwitches != 1 || stats.QueueWaitMS != 80 || stats.RetryWaitMS != 120 || stats.HTTPPhases["response_headers_ms"].AvgMS != 30 {
		t.Fatalf("lost observations: %+v", stats)
	}
	if _, ok := stats.HTTPPhases["tls_ms"]; ok {
		t.Fatal("invented missing TLS stage")
	}
	tracker.Track(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"answer":"ok"}`)) })(httptest.NewRecorder(), httptest.NewRequest("POST", "/grok/messages", nil))
	nonstream := tracker.Snapshot("grok").Nodes["grok"]
	if nonstream.FirstGeneration.Samples != 0 || nonstream.FirstGeneration.P95MS != nil || nonstream.FirstVisible.Samples != 0 || nonstream.BodyTTFB.Samples != 1 {
		t.Fatalf("nonstream invented generation: %+v", nonstream)
	}
}

func TestFlowCountsErrorsCancellationAndFinalCoalescedFlush(t *testing.T) {
	tracker := NewFlowTracker()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tracker.Track(func(http.ResponseWriter, *http.Request) {})(httptest.NewRecorder(), httptest.NewRequest("POST", "/qoder/messages", nil).WithContext(ctx))
	deadlineCtx, expire := context.WithCancelCause(context.Background())
	expire(context.DeadlineExceeded)
	tracker.Track(func(http.ResponseWriter, *http.Request) {})(httptest.NewRecorder(), httptest.NewRequest("POST", "/qoder/messages", nil).WithContext(deadlineCtx))
	tracker.Track(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429); w.Write([]byte(`{"error":"busy"}`)) })(httptest.NewRecorder(), httptest.NewRequest("POST", "/qoder/messages", nil))
	tracker.Track(CoalesceStreamFlush(func() time.Duration { return time.Second })(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
		w.(http.Flusher).Flush()
	}))(flowFlushFailureWriter{httptest.NewRecorder()}, httptest.NewRequest("POST", "/qoder/messages", nil))
	stats := tracker.Snapshot("qoder").Nodes["qoder"]
	if stats.Completed != 4 || stats.Failed != 4 || stats.Canceled != 1 || stats.Timeouts != 1 || stats.RateLimited != 1 || stats.StreamErrors != 1 || stats.Waiting != 0 || stats.Responding != 0 || *stats.FailureRate != 1 {
		t.Fatalf("failure was reported as success: %+v", stats)
	}
}

type flowFlushFailureWriter struct{ *httptest.ResponseRecorder }

func (w flowFlushFailureWriter) FlushError() error { return context.DeadlineExceeded }

func TestFlowSampleBoundAndPooledPercentile(t *testing.T) {
	tracker := NewFlowTracker()
	now := time.Now()
	// A low-volume fast route and a busy slow route must pool their samples;
	// averaging their individual P95 values would produce the wrong answer.
	for _, fixture := range []struct {
		key   string
		count int
		ms    int64
	}{{"cline:chat", 1, 10}, {"cline:responses", 300, 1000}} {
		route := tracker.routes[fixture.key]
		route.bucket(now).Completed = int64(fixture.count)
		for i := 0; i < fixture.count; i++ {
			route.samples[i%flowSampleLimit] = flowLatencySample{at: now, firstGeneration: fixture.ms, firstVisible: -1, bodyTTFB: -1, duration: fixture.ms}
		}
	}
	stats := tracker.Snapshot("cline").Nodes["cline"]
	if stats.Completed != 301 || stats.FirstGeneration.Samples != 257 || *stats.FirstGeneration.P95MS != 1000 || stats.FirstVisible.P95MS != nil {
		t.Fatalf("bad retention or pooled percentile: %+v", stats)
	}
	if _, ok := tracker.Snapshot("grok").Nodes["cline"]; ok {
		t.Fatal("scope leaked another channel")
	}
	for _, route := range tracker.routes {
		for i := range route.buckets {
			route.buckets[i].second -= 61
		}
		for i := range route.samples {
			if !route.samples[i].at.IsZero() {
				route.samples[i].at = now.Add(-61 * time.Second)
			}
		}
	}
	stats = tracker.Snapshot("cline").Nodes["gateway"]
	if stats.Completed != 0 || stats.FailureRate != nil || stats.FirstGeneration.P95MS != nil {
		t.Fatalf("stale data survived window: %+v", stats)
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
