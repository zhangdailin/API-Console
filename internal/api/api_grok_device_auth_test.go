package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/testutil"
)

// grokLoginTestAPI is the API the constructor builds, without a store: enough
// for a handler test to seed one device-login transaction of its choosing.
func grokLoginTestAPI() *API {
	return &API{grokLogins: newDeviceLoginRegistry(channel.Grok)}
}

func TestHandleGrokDeviceAuthorizationStatusRedactsDeviceCode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := grokLoginTestAPI()
	a.grokLogins.admit("login-id", &deviceLogin{
		challenge: provider.LoginChallenge{DeviceCode: "must-not-be-exposed", UserCode: "ABCD-1234"},
		verifyURI: "https://auth.x.ai/device",
		expiresAt: time.Now().Add(time.Minute), status: "pending", cancel: cancel,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/grok/device-auth/login-id", nil).WithContext(ctx)
	a.HandleGrokDeviceAuthorization(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)
	body := rec.Body.String()
	testutil.Falsef(t, strings.Contains(body, "must-not-be-exposed") || !strings.Contains(body, "ABCD-1234"), "unexpected response: %s", body)
}

func TestHandleGrokDeviceAuthorizationDeleteCancelsAndForgets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := grokLoginTestAPI()
	a.grokLogins.admit("login-id", &deviceLogin{
		challenge: provider.LoginChallenge{DeviceCode: "device-secret"},
		expiresAt: time.Now().Add(time.Minute), status: "pending", cancel: cancel,
	})
	rec := httptest.NewRecorder()
	a.HandleGrokDeviceAuthorization(rec, httptest.NewRequest(http.MethodDelete, "/api/grok/device-auth/login-id", nil))
	testutil.Equal(t, rec.Code, http.StatusNoContent)
	var remained bool
	a.grokLogins.update("login-id", func(*deviceLogin) { remained = true })
	testutil.False(t, remained, "cancelled login remained in memory")
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel function was not called")
	}
}

func TestGrokDeviceLoginStartsWithPublicSnapshotAndKeepsOriginalFlow(t *testing.T) {
	store, _ := newTestStore(t, "grok-login-snapshot:")
	api := New(store, "", "", &config.Config{})
	originalConfig := api.config.Load()
	var resolutions atomic.Int32
	flow := &provider.Login{
		Start: func(context.Context, *config.Config) (provider.LoginChallenge, error) {
			api.config.Store(&config.Config{})
			return provider.LoginChallenge{
				DeviceCode: "private-device-code", UserCode: "ABCD-1234",
				ExpiresAt: time.Now().Add(time.Minute),
			}, nil
		},
		Exchange: func(_ context.Context, cfg *config.Config, challenge provider.LoginChallenge) (provider.LoginGrant, error) {
			if cfg != originalConfig || challenge.DeviceCode != "private-device-code" {
				t.Error("poll did not receive the original configuration and challenge")
			}
			return nil, errors.New("fixture terminal reply")
		},
		Build: registeredLoginFlow(channel.Grok).Build,
	}
	resolveLoginFlowMu.Lock()
	previous := resolveLoginFlow
	resolveLoginFlow = func(id channel.ID) *provider.Login {
		if id != channel.Grok {
			return previous(id)
		}
		if resolutions.Add(1) > 1 {
			return nil
		}
		return flow
	}
	resolveLoginFlowMu.Unlock()
	t.Cleanup(func() {
		resolveLoginFlowMu.Lock()
		resolveLoginFlow = previous
		resolveLoginFlowMu.Unlock()
	})

	rec := httptest.NewRecorder()
	api.HandleGrokDeviceAuthorization(rec, httptest.NewRequest(http.MethodPost, "/api/grok/device-auth", nil))
	var start deviceLoginResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &start) != nil ||
		start.Status != "pending" || start.UserCode != "ABCD-1234" || strings.Contains(rec.Body.String(), "private-device-code") {
		t.Fatalf("unexpected start response: %d %s", rec.Code, rec.Body.String())
	}
	var done <-chan struct{}
	api.grokLogins.update(start.ID, func(login *deviceLogin) { done = login.done })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("login poll did not finish")
	}
	final, ok := api.grokLogins.response(start.ID)
	if !ok || final.Status != "failed" || final.UserCode != "" || resolutions.Load() != 1 {
		t.Fatalf("unexpected final login state: %+v, provider resolutions: %d", final, resolutions.Load())
	}
}
