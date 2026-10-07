package middleware

import (
	"net/http"
	"orchids-api/internal/config"
	"orchids-api/internal/util"
)

// InferenceBudget applies only to generation routes, preserving parent deadlines.
func InferenceBudget(current func() *config.Config) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || requestChannel(r.URL.Path) == HTTPChannel {
				next(w, r)
				return
			}
			ctx, cancel := util.WithFirstGenerationTimeout(r.Context(), current().FirstGenerationTimeout())
			defer cancel()
			next(w, r.WithContext(ctx))
		}
	}
}
