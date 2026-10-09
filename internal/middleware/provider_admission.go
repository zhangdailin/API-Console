package middleware

import (
	"net/http"
	"strings"
	"sync/atomic"

	"orchids-api/internal/channel"
)

// Both base paths for a provider share its independent capacity budget.
func ProviderAdmission(limits func() map[string]int) func(http.HandlerFunc) http.HandlerFunc {
	// One counter per channel, built from the channel table rather than a
	// hardcoded list: a removed channel loses its slot, and an added one gains
	// one without this file changing. channel.ID is the URL's first segment
	// ("/workbuddy/v1" -> "workbuddy"), which is exactly the key looked up below.
	counts := map[string]*atomic.Int64{}
	for _, definition := range channel.All() {
		counts[string(definition.ID)] = &atomic.Int64{}
	}
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			name, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
			count := counts[name]
			if count == nil || limits == nil {
				next(w, r)
				return
			}
			limit := limits()[name]
			if limit <= 0 {
				next(w, r)
				return
			}
			for {
				n := count.Load()
				if n >= int64(limit) {
					w.Header().Set("Retry-After", "1")
					writeAPIKeyError(w, http.StatusServiceUnavailable, "provider is overloaded; retry later", "provider_overloaded")
					return
				}
				if count.CompareAndSwap(n, n+1) {
					break
				}
			}
			defer count.Add(-1)
			next(w, r)
		}
	}
}
