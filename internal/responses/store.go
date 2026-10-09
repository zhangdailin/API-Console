package responses

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

// Store is the persistence the Responses layer needs to save a response, serve
// it back and delete it. *store.Store satisfies it against Redis;
// store.MemoryResponseStore satisfies it in-process.
type Store interface {
	SaveStoredResponse(ctx context.Context, response *store.StoredResponse, ttl time.Duration) error
	GetStoredResponse(ctx context.Context, responseID, ownerHash string) (*store.StoredResponse, error)
	DeleteStoredResponse(ctx context.Context, responseID, ownerHash string) error
}

// BridgeOptions configures the bridge's response store.
//
// The store is what makes store=true, previous_response_id and
// GET/DELETE /responses/{id} work on channels that have no native Responses
// storage. Without one the bridge still serves stateless requests, which is what
// Codex does by default (it resends the whole conversation every turn).
type BridgeOptions struct {
	// Store is the shared response store. Nil falls back to the process-wide
	// in-process store, so the bridge keeps store=true, previous_response_id and
	// resource retrieval working on a gateway started without a response
	// backend. A multi-replica gateway must pass the shared store: an in-process
	// record is only visible to the replica that wrote it.
	Store Store
	// TTL overrides the stored-response lifetime; zero uses the default.
	TTL time.Duration
}

// memoryFallbackWarned keeps the "no shared store" warning to one line per
// process instead of one per request.
var memoryFallbackWarned sync.Once

// Store resolves the response store the bridge reads and writes. It is never
// nil: a gateway with no response backend gets the in-process fallback rather
// than a hard failure on every stored-response request.
func (o BridgeOptions) store() Store {
	if o.Store != nil {
		return o.Store
	}
	memoryFallbackWarned.Do(func() {
		slog.Warn("Responses bridge has no shared response store; falling back to an in-process store. " +
			"stored responses are only visible to this process — configure Redis for a multi-replica deployment")
	})
	return store.DefaultMemoryResponseStore()
}

// StoreFor is the exported form of store, for callers outside the package.
func (o BridgeOptions) StoreFor() Store { return o.store() }

// TTLOrDefault resolves the stored-response lifetime.
func (o BridgeOptions) TTLOrDefault() time.Duration {
	if o.TTL > 0 {
		return o.TTL
	}
	return DefaultStoredResponseTTL
}

// OwnerHash identifies the caller that owns a stored response, so one key
// cannot read or delete another key's records.
func OwnerHash(ctx context.Context) string {
	if owner := strings.TrimSpace(middleware.APIKeyFingerprint(ctx)); owner != "" {
		return owner
	}
	return "anonymous"
}

// ResponseIDFromResourcePath reads the response id out of a resource path.
func ResponseIDFromResourcePath(path string) string {
	marker := "/responses/"
	index := strings.LastIndex(path, marker)
	if index < 0 {
		return ""
	}
	value := strings.Trim(strings.TrimSpace(path[index+len(marker):]), "/")
	if value == "" || strings.Contains(value, "/") || strings.EqualFold(value, "compact") {
		return ""
	}
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(decoded)
}

// WriteStoredLookupError answers a store lookup failure: a missing row is a
// 404 the caller can act on, anything else is an unavailable store.
func WriteStoredLookupError(w http.ResponseWriter, err error, notFoundMessage string) {
	if errors.Is(err, store.ErrNoRows) {
		WriteAPIError(w, http.StatusNotFound, "response_not_found", notFoundMessage)
		return
	}
	WriteAPIError(w, http.StatusServiceUnavailable, "response_store_unavailable", "response store unavailable")
}
