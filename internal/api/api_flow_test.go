package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"orchids-api/internal/middleware"
	"orchids-api/internal/testutil"
)

type flowStreamRecorder struct {
	*httptest.ResponseRecorder
	mu sync.Mutex
}

func (w *flowStreamRecorder) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ResponseRecorder.Write(data)
}

func (w *flowStreamRecorder) frameCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Count(w.Body.String(), "event: flow\n")
}

func TestOpsFlowCoalescesBurstsAndPreservesHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tracker := middleware.NewFlowTracker()
		a := &API{flowTracker: tracker}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		response := &flowStreamRecorder{ResponseRecorder: httptest.NewRecorder()}
		request := httptest.NewRequest(http.MethodGet, "/api/ops/flow?channel=workbuddy", nil).WithContext(ctx)
		done := make(chan struct{})
		started := time.Now()
		go func() {
			defer close(done)
			a.HandleOpsFlow(response, request)
		}()
		synctest.Wait()
		frames := response.frameCount
		testutil.Equal(t, frames(), 1)
		checkBound := func() {
			t.Helper()
			maximum := 1 + int(time.Since(started)/flowPushInterval)
			testutil.Falsef(t, frames() > maximum, "got %d frames in %s; maximum %d", frames(), time.Since(started), maximum)
		}
		track := tracker.Track(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
		// Repeated start, first-write and finish notifications cross the heartbeat
		// boundary while staying much more frequent than the push interval.
		for range 15 {
			for range 100 {
				track(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/workbuddy/v1/chat/completions", nil))
			}
			synctest.Wait()
			checkBound()
			time.Sleep(flowPushInterval / 2)
			synctest.Wait()
			checkBound()
		}
		testutil.True(t, frames() > 1, "event snapshots were never delivered")
		// Drain the final burst before checking that idle heartbeats still send.
		time.Sleep(2 * flowPushInterval)
		synctest.Wait()
		beforeHeartbeat := frames()
		time.Sleep(time.Second)
		synctest.Wait()
		testutil.True(t, frames() > beforeHeartbeat, "idle heartbeat was lost")
		checkBound()
		cancel()
		<-done
	})
}

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
	if initial := read(); len(initial.Routes) != 3 || initial.Routes[0].Active != 0 || initial.RateSeconds != 5 || initial.WindowSeconds != 60 || initial.LatencyWindowSeconds != 900 || initial.SampleLimit != 256 || initial.Nodes["workbuddy"].FailureRate != nil || initial.Nodes["workbuddy"].FirstGeneration.P95MS != nil {
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
