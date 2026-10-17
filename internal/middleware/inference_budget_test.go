package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
	"orchids-api/internal/util"
)

func TestInferenceBudgetUsesSelectedProfile(t *testing.T) {
	for _, profile := range []string{"standard", "interactive"} {
		t.Run(profile, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := &config.Config{InferenceProfile: profile}
				started := time.Now()
				h := InferenceBudgetWithConfig(func(_ http.ResponseWriter, r *http.Request) {
					<-r.Context().Done()
					testutil.Equal(t, context.Cause(r.Context()), error(context.DeadlineExceeded))
				}, func() *config.Config { return cfg })
				h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/qoder/v1/chat/completions", nil))
				testutil.Equal(t, time.Since(started), cfg.InferencePolicy().FirstGeneration)
			})
		})
	}
}

func TestInferenceProgressRetainsParentDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		started := time.Now()
		h := InferenceBudgetWithConfig(func(_ http.ResponseWriter, r *http.Request) {
			util.MarkGenerationProgress(r.Context())
			<-r.Context().Done()
		}, func() *config.Config { return &config.Config{InferenceProfile: "interactive"} })
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/grok/v1/responses", nil).WithContext(parent))
		testutil.Equal(t, time.Since(started), time.Minute)
	})
}
