package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/channel"
)

const flowPushInterval = 200 * time.Millisecond

// HandleOpsFlow streams coalesced inference counters with periodic idle updates.
// Historical Redis aggregation is not on this path.
func (a *API) HandleOpsFlow(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	scope := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("channel")))
	if scope != "" && !channel.IsSupported(scope) {
		http.Error(w, "invalid channel", http.StatusBadRequest)
		return
	}
	if a.flowTracker == nil {
		http.Error(w, "flow tracking unavailable", http.StatusServiceUnavailable)
		return
	}
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	// Keep proxies from compressing or transforming tiny event frames into
	// buffered batches; each emitted snapshot must reach the browser promptly.
	w.Header().Set("Cache-Control", "no-cache, no-store, no-transform")
	w.Header().Set("Content-Encoding", "identity")
	w.Header().Set("X-Accel-Buffering", "no")
	events, unsubscribe := a.flowTracker.Subscribe()
	defer unsubscribe()
	defer controller.SetWriteDeadline(time.Time{})
	updates := time.NewTicker(flowPushInterval)
	defer updates.Stop()
	heartbeat := time.NewTicker(time.Second)
	defer heartbeat.Stop()
	var lastSentAt time.Time
	send := func() error {
		controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		data, err := json.Marshal(a.flowTracker.Snapshot(scope))
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(w, "event: flow\ndata: %s\n\n", data); err != nil {
			return err
		}
		if err := controller.Flush(); err != nil {
			return err
		}
		lastSentAt = time.Now()
		updates.Reset(flowPushInterval)
		return nil
	}
	if send() != nil {
		return
	}
	dirty := false
	for {
		select {
		case <-r.Context().Done():
			return
		case <-events:
			dirty = true
		case <-updates.C:
			if dirty && time.Since(lastSentAt) >= flowPushInterval {
				if send() != nil {
					return
				}
				dirty = false
			}
		case <-heartbeat.C:
			// Heartbeats share the event rate limit, including coincident ticks.
			if time.Since(lastSentAt) >= flowPushInterval {
				if send() != nil {
					return
				}
				dirty = false
			} else {
				dirty = true
			}
		}
	}
}
