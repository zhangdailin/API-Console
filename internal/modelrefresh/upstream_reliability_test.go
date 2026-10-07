package modelrefresh

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/cline"
	"orchids-api/internal/config"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
	"orchids-api/internal/workbuddy"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpstreamReliabilityModelDiscoveryPersistsRotatedCredentials(t *testing.T) {
	for _, channel := range []string{"qoder", "workbuddy"} {
		t.Run(channel, func(t *testing.T) {
			s, cleanup := setupModelRefreshStore(t)
			defer cleanup()
			rotations := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "deviceToken/refresh"):
					rotations++
					io.WriteString(w, "{\"device_token\":\"new-access\",\"refresh_token\":\"rotated-refresh\",\"expires_in\":7200}")
				case strings.Contains(r.URL.Path, "auth/token/refresh"):
					rotations++
					io.WriteString(w, "{\"code\":0,\"data\":{\"accessToken\":\"new-access\",\"refreshToken\":\"rotated-refresh\",\"expiresIn\":7200}}")
				case strings.Contains(r.URL.Path, "model/list"):
					io.WriteString(w, "{\"chat\":[{\"key\":\"model-a\",\"name\":\"Model A\",\"format\":\"openai\",\"source\":\"system\",\"enable\":true}]}")
				case r.URL.Path == "/v3/config":
					io.WriteString(w, "{\"code\":0,\"data\":{\"models\":[{\"id\":\"model-a\"}]}}")
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			acc := qoderTestAccount("11111111-2222-4333-8444-555555555555")
			acc.QoderExpiresAt = time.Now().Add(-time.Hour)
			if channel == "workbuddy" {
				acc = &store.Account{AccountType: "workbuddy", Enabled: true, Weight: 1, WorkBuddyAccessToken: "old-access", WorkBuddyRefreshToken: "old-refresh", WorkBuddyExpiresAt: time.Now().Add(-time.Hour), WorkBuddyUID: "uid"}
			}
			if err := s.CreateAccount(context.Background(), acc); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{QoderInferenceURL: server.URL, QoderOpenAPIBaseURL: server.URL, WorkBuddyBaseURL: server.URL}
			report, err := discoverAccountCatalogModels(context.Background(), cfg, s, channel, 1)
			if err != nil || len(report.Candidates) == 0 {
				t.Fatalf("catalog failed: %v; report=%+v", err, report)
			}
			got, err := s.GetAccount(context.Background(), acc.ID)
			if err != nil {
				t.Fatal(err)
			}
			saved := got.QoderRefreshToken
			if channel == "workbuddy" {
				saved = got.WorkBuddyRefreshToken
			}
			t.Logf("upstream rotations=%d catalog-success=true", rotations)
			if saved != "rotated-refresh" {
				t.Errorf("successful %s catalog discovery discarded rotated refresh credential", channel)
			}
		})
	}
}

func TestCredentialRotationIsSharedAcrossClientInstances(t *testing.T) {
	for _, channel := range []string{"qoder", "workbuddy", "cline"} {
		t.Run(channel, func(t *testing.T) {
			s, cleanup := setupModelRefreshStore(t)
			defer cleanup()
			var rotations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if channel == "qoder" {
						io.WriteString(w, `{"chat":[{"key":"model-a","name":"Model A","source":"system","enable":true}]}`)
					} else {
						io.WriteString(w, `{"free":[{"id":"model-a"}]}`)
					}
					return
				}
				rotations.Add(1)
				switch channel {
				case "qoder":
					io.WriteString(w, `{"device_token":"new-access","refresh_token":"rotated-refresh","expires_in":7200}`)
				case "workbuddy":
					io.WriteString(w, `{"code":0,"data":{"accessToken":"new-access","refreshToken":"rotated-refresh","expiresIn":7200}}`)
				case "cline":
					io.WriteString(w, `{"data":{"accessToken":"new-access","refreshToken":"rotated-refresh","expiresAt":"2099-01-01T00:00:00Z"}}`)
				}
			}))
			defer server.Close()
			acc := &store.Account{AccountType: channel, Enabled: true, Weight: 1}
			switch channel {
			case "qoder":
				acc = qoderTestAccount("11111111-2222-4333-8444-555555555555")
				acc.QoderExpiresAt = time.Now().Add(-time.Hour)
			case "workbuddy":
				acc.WorkBuddyRefreshToken = "old-refresh"
				acc.WorkBuddyAccessToken = "old-access"
				acc.WorkBuddyExpiresAt = time.Now().Add(-time.Hour)
			case "cline":
				acc.ClineRefreshToken = "old-refresh"
				acc.ClineAccessToken = "old-access"
				acc.ClineExpiresAt = time.Now().Add(-time.Hour)
			}
			if err := s.CreateAccount(context.Background(), acc); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{QoderOpenAPIBaseURL: server.URL, QoderInferenceURL: server.URL, WorkBuddyBaseURL: server.URL, ClineAPIBaseURL: server.URL}
			var functions []func() error
			for range 8 {
				switch channel {
				case "qoder":
					c := qoder.NewFromAccount(acc, cfg)
					c.SetAccountStore(s)
					functions = append(functions, func() error { _, err := c.FetchUpstreamModels(context.Background()); return err })
				case "workbuddy":
					c := workbuddy.NewFromAccount(acc, cfg)
					c.SetAccountStore(s)
					functions = append(functions, func() error { _, err := c.RefreshCredentials(context.Background()); return err })
				case "cline":
					c := cline.NewFromAccount(acc, cfg)
					c.SetAccountStore(s)
					functions = append(functions, func() error { _, err := c.FetchUpstreamModels(context.Background()); return err })
				}
			}
			var wg sync.WaitGroup
			errs := make(chan error, len(functions))
			for _, f := range functions {
				wg.Add(1)
				go func() { defer wg.Done(); errs <- f() }()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if rotations.Load() != 1 {
				t.Fatalf("same account rotated %d times across clients", rotations.Load())
			}
		})
	}
}
