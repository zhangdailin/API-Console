package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

// TestRegisterRoutes_ResponsesSubResources proves the endpoints below a response
// id are reachable through the real route table on every prefix.
//
// They are registered explicitly rather than through the model dispatcher
// because a cancel body carries no model: dispatching them by body would send
// the same request to the native handler or the bridge depending on whether the
// client sent `{}` or nothing. A 404 page-not-found here would mean the route is
// missing; the response_not_found envelope means the route exists and the store
// simply has no such record.
func TestRegisterRoutes_ResponsesSubResources(t *testing.T) {
	// Inference auth is unconditional: no switch opens /v1, so the
	// probe has to carry a managed key to reach the routing layer.
	cfg := &config.Config{
		AdminUser: "admin", AdminPass: "secret", AdminToken: "admintoken", AdminPath: "/admin",
	}
	// A managed key, because /v1 requires one unconditionally.
	e := newChannelE2E(t, "responses-routes:", "sk-responses-subresource", cfg)

	for _, prefix := range channel.AllPrefixes() {
		t.Run(prefix, func(t *testing.T) {
			for _, probe := range []struct {
				method string
				path   string
				body   string
			}{
				{http.MethodPost, prefix + "/responses/resp_absent/cancel", "{}"},
				{http.MethodPost, prefix + "/responses/resp_absent/cancel", ""},
				{http.MethodGet, prefix + "/responses/resp_absent/input_items", ""},
			} {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(probe.method, probe.path, strings.NewReader(probe.body))
				req.Header.Set("Authorization", "Bearer "+e.managedKey)
				e.mux.ServeHTTP(rec, req)

				testutil.NotEqual(t, rec.Code, http.StatusMethodNotAllowed)
				testutil.MustNotContain(t, rec.Body.String(), "404 page not found")
				testutil.Equal(t, rec.Code, http.StatusNotFound)
				testutil.MustContain(t, rec.Body.String(), "response_not_found")
			}

			// A wrong method must still be answered by the handler (405 with an
			// Allow header), which is the difference between "this endpoint
			// rejects GET" and "this endpoint does not exist".
			rec := httptest.NewRecorder()
			wrongMethod := httptest.NewRequest(http.MethodGet, prefix+"/responses/resp_absent/cancel", nil)
			wrongMethod.Header.Set("Authorization", "Bearer "+e.managedKey)
			e.mux.ServeHTTP(rec, wrongMethod)
			testutil.Equal(t, rec.Code, http.StatusMethodNotAllowed)
			allow := rec.Header().Get("Allow")
			testutil.Falsef(t, !strings.Contains(allow, http.MethodPost), "Allow = %q, want POST", allow)
		})
	}

	// The response id itself must keep working on the provider prefix: the
	// explicit sibling routes must not shadow the /responses/ subtree.
	rec := httptest.NewRecorder()
	resourceReq := httptest.NewRequest(http.MethodGet, "/workbuddy/responses/resp_absent", nil)
	resourceReq.Header.Set("Authorization", "Bearer "+e.managedKey)
	e.mux.ServeHTTP(rec, resourceReq)
	testutil.Falsef(t, rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "response_not_found"), "GET /v1/responses/resp_absent status = %d body = %s", rec.Code, rec.Body.String())
}
