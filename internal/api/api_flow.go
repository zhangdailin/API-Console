package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/channel"
)

// HandleOpsFlow streams current inference counters as soon as handlers start or
// finish. Historical Redis aggregation is not on this path.
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
	// buffered batches; each request transition must reach the browser now.
	w.Header().Set("Cache-Control", "no-cache, no-store, no-transform")
	w.Header().Set("Content-Encoding", "identity")
	w.Header().Set("X-Accel-Buffering", "no")
	events, unsubscribe := a.flowTracker.Subscribe()
	defer unsubscribe()
	defer controller.SetWriteDeadline(time.Time{})
	heartbeat := time.NewTicker(time.Second)
	defer heartbeat.Stop()
	send := func() error {
		controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		data, err := json.Marshal(a.flowTracker.Snapshot(scope))
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(w, "event: flow\ndata: %s\n\n", data); err != nil {
			return err
		}
		return controller.Flush()
	}
	if send() != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-events:
			if send() != nil {
				return
			}
		case <-heartbeat.C:
			if send() != nil {
				return
			}
		}
	}
}
