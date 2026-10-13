package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/middleware"
)

func TestOpsFlowPushesStartAndFinish(t *testing.T) {
	tracker := middleware.NewFlowTracker()
	a := &API{flowTracker: tracker}
	server := httptest.NewServer(http.HandlerFunc(a.HandleOpsFlow))
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(server.URL + "?channel=workbuddy")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("X-Accel-Buffering") != "no" || response.Header.Get("Content-Encoding") != "identity" || !strings.Contains(response.Header.Get("Cache-Control"), "no-transform") {
		t.Fatal("stream headers missing")
	}
	scanner := bufio.NewScanner(response.Body)
	read := func() middleware.FlowSnapshot {
		t.Helper()
		for scanner.Scan() {
			if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
				var snapshot middleware.FlowSnapshot
				if err := json.Unmarshal([]byte(data), &snapshot); err != nil {
					t.Fatal(err)
				}
				return snapshot
			}
		}
		t.Fatalf("stream ended: %v", scanner.Err())
		return middleware.FlowSnapshot{}
	}
	if initial := read(); len(initial.Routes) != 3 || initial.Routes[0].Active != 0 || initial.RateSeconds != 5 || initial.WindowSeconds != 60 || initial.SampleLimit != 256 || initial.Nodes["workbuddy"].FailureRate != nil || initial.Nodes["workbuddy"].FirstGeneration.P95MS != nil {
		t.Fatalf("initial: %+v", initial)
	}
	release, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tracker.Track(func(http.ResponseWriter, *http.Request) { <-release })(httptest.NewRecorder(), httptest.NewRequest("POST", "/workbuddy/v1/chat/completions", nil))
	}()
	if started := read().Routes[0]; started.Active != 1 || started.Started != 1 {
		t.Fatalf("start: %+v", started)
	}
	close(release)
	<-done
	ended := read()
	stats := ended.Nodes["gateway"]
	if ended.Routes[0].Active != 0 || ended.Routes[0].Completed != 1 || stats.Completed != 1 || stats.Waiting != 0 || stats.FailureRate == nil || *stats.FailureRate != 0 {
		t.Fatalf("finish: %+v", ended)
	}
}
