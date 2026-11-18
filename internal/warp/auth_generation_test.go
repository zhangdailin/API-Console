package warp

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func TestAuthenticationFallbackDoesNotReplayGeneration(t *testing.T) {
	for _, status := range []int{200, 401} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			acc := testAccount()
			acc.WarpExpiresAt = time.Now().Add(-time.Hour)
			c := NewFromAccount(acc, &config.Config{WarpFirebaseAPIKey: "synthetic-project-config"})
			direct, proxy, paid := 0, 0, 0
			stream := testSSE(t, testMessages(testOutput("auth-fallback", "safe answer"))) + testSSE(t, testDone())
			c.http.Transport = diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Host + r.URL.Path {
				case "securetoken.googleapis.com/v1/token":
					direct++
					return authResponse(503, `{}`), nil
				case "app.warp.dev/proxy/token":
					proxy++
					return authResponse(200, `{"id_token":"synthetic-renewed","refresh_token":"synthetic-rotated","expires_in":"3600"}`), nil
				case "app.warp.dev/ai/multi-agent":
					paid++
					if r.Header.Get("Authorization") != "Bearer synthetic-renewed" {
						t.Error("generation used an unrefreshed credential")
					}
					response := authResponse(status, stream)
					response.Header.Set("Content-Type", "text/event-stream")
					return response, nil
				default:
					t.Error("unexpected network destination")
					return nil, errors.New("unexpected request")
				}
			})
			ctx, budget := upstream.WithAttemptBudget(t.Context(), 1)
			err := c.SendRequestWithPayload(ctx, testRequest(), nil, nil)
			if (err == nil) != (status == 200) || direct != 1 || proxy != 1 || paid != 1 || budget.Used() != 1 {
				t.Fatalf("generation replayed or bypassed auth: direct=%d proxy=%d paid=%d attempts=%d err=%v", direct, proxy, paid, budget.Used(), err)
			}
		})
	}
}

func TestMaintenanceRecordsTerminalCredentialVerdict(t *testing.T) {
	s := newAuthStateTestStore(t)
	acc := testAccount()
	acc.WarpExpiresAt = time.Now().Add(-time.Hour)
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/token" {
			t.Error("terminal credentials reached catalog or generation")
		}
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"message":"TOKEN_EXPIRED"}}`)
	}))
	defer srv.Close()
	maintain(t.Context(), provider.Env{Config: testConfig(srv.URL), Store: s}, acc)
	got, err := s.GetAccount(t.Context(), acc.ID)
	if err != nil || store.AccountAuthActive(got) || got.StatusCode != "401" || calls != 1 || !strings.Contains(got.StatusMessage, "TOKEN_EXPIRED") {
		t.Fatalf("maintenance did not record auth verdict: calls=%d err=%v", calls, err)
	}
}
