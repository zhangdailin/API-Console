package grok

import (
	"net/http"
	"strings"
	"sync"
)

// The Build gateway stamps x-models-etag on responses. A change against the
// etag of the last catalog read means the account's catalog moved, so the
// next maintenance pass reads it again instead of waiting for the snapshot
// TTL. The state is in-process: after a restart the first etag seen marks the
// account once, and the read that follows records the baseline.
type modelsETagTracker struct {
	mu       sync.Mutex
	baseline map[int64]string
	stale    map[int64]bool
}

var buildModelsETags = &modelsETagTracker{baseline: map[int64]string{}, stale: map[int64]bool{}}

// recordCatalogRead sets the baseline after a successful catalog read.
func (t *modelsETagTracker) recordCatalogRead(accountID int64, header http.Header) {
	if accountID == 0 {
		return
	}
	etag := strings.TrimSpace(header.Get("x-models-etag"))
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.stale, accountID)
	if etag != "" {
		t.baseline[accountID] = etag
	}
}

// observe compares an inference response's etag with the baseline.
func (t *modelsETagTracker) observe(accountID int64, header http.Header) {
	if accountID == 0 || header == nil {
		return
	}
	etag := strings.TrimSpace(header.Get("x-models-etag"))
	if etag == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.baseline[accountID] != etag {
		t.stale[accountID] = true
	}
}

func (t *modelsETagTracker) isStale(accountID int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stale[accountID]
}
