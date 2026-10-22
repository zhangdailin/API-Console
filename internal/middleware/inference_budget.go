package middleware

import (
	"net/http"
	"orchids-api/internal/config"
	"orchids-api/internal/util"
)

// InferenceBudgetWithConfig snapshots the deployment profile for this request.
func InferenceBudgetWithConfig(next http.HandlerFunc, snapshot func() *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || requestChannel(r.URL.Path) == HTTPChannel {
			next(w, r)
			return
		}
		var cfg *config.Config
		if snapshot != nil {
			cfg = snapshot()
		}
		policy := cfg.InferencePolicy()
		ctx, cancel := util.WithFirstGenerationTimeout(r.Context(), policy.FirstGeneration)
		defer cancel()
		AnnotateRequest(ctx, "inference_profile", policy.Name)
		next(w, r.WithContext(ctx))
	}
}
