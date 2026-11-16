package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func registeredWarpLoginForTest(t *testing.T) *provider.Login {
	t.Helper()
	spec, ok := provider.Lookup("warp")
	testutil.True(t, ok, "Warp must be linked through the provider registry")
	testutil.True(t, spec.Admin != nil && spec.Admin.Login != nil, "Warp must register its login flow")
	flow := spec.Admin.Login
	testutil.True(t, flow.Start != nil && flow.Exchange != nil && flow.Build != nil, "Warp login must implement the complete flow")
	testutil.True(t, flow.Merge != nil && flow.MarkReplace != nil, "Warp re-login must register merge and credential replacement")
	return flow
}

func assertWarpLoginResponseIsSafe(t *testing.T, raw []byte, private ...string) {
	t.Helper()
	var payload map[string]json.RawMessage
	testutil.NoError(t, json.Unmarshal(raw, &payload), "decode login response: %v")
	for _, key := range []string{
		"device_code", "challenge", "private", "grant", "account",
		"access_token", "refresh_token", "token", "client_cookie",
		"warp_access_token", "warp_refresh_token", "firebase_api_key", "warp_firebase_api_key",
		"warp_quota", "quota_status", "quota_windows",
	} {
		_, exists := payload[key]
		testutil.Falsef(t, exists, "private field %q reached the login response", key)
	}
	for _, value := range private {
		testutil.MustNotContain(t, string(raw), value)
	}
}

// Exercise the registered provider rather than importing a channel package. Both
// completion paths must merge a rotated grant by stable identity, not token.
func TestWarpLoginIntegration_ReloginPreservesOperatorState(t *testing.T) {
	for _, completion := range []string{"finishBrowserLogin", "pollChannelLogin"} {
		for _, quotaFailure := range []bool{false, true} {
			name := completion + "/quota_ok"
			if quotaFailure {
				name = completion + "/quota_failure"
			}
			t.Run(name, func(t *testing.T) {
				flow := registeredWarpLoginForTest(t)
				s, _ := newTestStore(t, "warp-login-integration:")
				now := time.Now().UTC().Truncate(time.Second)
				existing := &store.Account{
					AccountType: "warp", CredentialType: "oauth", UserID: "warp-integration-user",
					Name: "operator-owned name", Email: "old@example.invalid", Enabled: false,
					Weight: 17, MaxConcurrent: 3,
					StatusCode: "401", AuthStatus: store.AccountAuthStatusReauthRequired,
					StatusMessage: "The previous Warp grant was rejected; re-login is required",
					LastAttempt:   now.Add(-time.Hour), QuotaResetAt: now.Add(time.Hour),
					RequestCount: 29, UsageTotal: 9876, TokensToday: 1234, TokensDate: now.Format("2006-01-02"),
					UsageCurrent: 42, UsageLimit: 100, LastUsedAt: now.Add(-time.Minute),
					WarpAccessToken: "synthetic-old-access", WarpRefreshToken: "synthetic-old-refresh",
					WarpExpiresAt: now.Add(-time.Hour), WarpModelIDs: []string{"old-model"},
					WarpModelsSyncedAt: now.Add(-24 * time.Hour), VerifiedAt: now.Add(-time.Hour),
				}
				testutil.NoError(t, s.CreateAccount(t.Context(), existing), "seed account: %v")
				before, err := s.GetAccount(t.Context(), existing.ID)
				testutil.NoError(t, err, "read seeded account: %v")
				testutil.Equal(t, before.StatusCode, "401")
				testutil.Equal(t, before.AuthStatus, store.AccountAuthStatusReauthRequired)
				testutil.NotEqual(t, before.StatusMessage, "")
				testutil.False(t, before.LastAttempt.IsZero(), "fixture must have a stale auth attempt")
				testutil.True(t, before.QuotaResetAt.After(now), "fixture must have a stale cooldown")

				const deviceCode = "synthetic-warp-device-private"
				const customToken = "synthetic-warp-custom-private"
				const refreshToken = "synthetic-warp-fresh-refresh"
				const projectKey = "synthetic-warp-project-config"
				accessToken := testJWT(existing.UserID, "fresh@example.invalid")
				const quotaPrivate = "synthetic-warp-private-workspace"
				const quotaError = "synthetic-warp-private-billing-error"
				var starts, exchanges, firebase, catalogs, quotas atomic.Int64
				auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					testutil.CheckEqual(t, r.Method, http.MethodPost)
					switch r.URL.Path {
					case "/api/v1/oauth/device/auth":
						starts.Add(1)
						testutil.CheckEqual(t, r.FormValue("client_id"), "warp-agent-cli")
						_, _ = w.Write([]byte(`{"device_code":"` + deviceCode + `","user_code":"WARP-TEST","verification_uri":"https://app.warp.dev/device","verification_uri_complete":"https://app.warp.dev/device?code=WARP-TEST","expires_in":600,"interval":2}`))
					case "/api/v1/oauth/token":
						exchanges.Add(1)
						testutil.CheckEqual(t, r.FormValue("client_id"), "warp-agent-cli")
						testutil.CheckEqual(t, r.FormValue("device_code"), deviceCode)
						testutil.CheckEqual(t, r.FormValue("grant_type"), "urn:ietf:params:oauth:grant-type:device_code")
						_, _ = w.Write([]byte(`{"access_token":"` + customToken + `"}`))
					case "/v1/accounts:signInWithCustomToken":
						firebase.Add(1)
						testutil.CheckEqual(t, r.URL.Query().Get("key"), projectKey)
						testutil.CheckEqual(t, r.FormValue("token"), customToken)
						testutil.CheckEqual(t, r.FormValue("returnSecureToken"), "true")
						_, _ = w.Write([]byte(`{"idToken":"` + accessToken + `","refreshToken":"` + refreshToken + `","expiresIn":"3600"}`))
					case "/graphql/v2":
						testutil.CheckEqual(t, r.Header.Get("Authorization"), "Bearer "+accessToken)
						switch r.URL.Query().Get("op") {
						case "GetFeatureModelChoices":
							catalogs.Add(1)
							_, _ = w.Write([]byte(`{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"featureModelChoice":{"agentMode":{"choices":[{"id":"new-model-b"},{"id":"new-model-a"}]}}}]}}}}`))
						case "GetRequestLimitInfo":
							quotas.Add(1)
							testutil.CheckEqual(t, catalogs.Load(), int64(1))
							if quotaFailure {
								http.Error(w, quotaError, http.StatusServiceUnavailable)
								return
							}
							_, _ = w.Write([]byte(`{"data":{"user":{"__typename":"UserOutput","user":{"email":"` + quotaPrivate + `","workspaces":[{"id":"` + quotaPrivate + `","name":"` + quotaPrivate + `","includedUsageInfo":{"includedUsageCredits":100,"usageCreditsUsedSinceLastRefresh":25,"nextRefreshTime":"2027-01-02T03:04:05Z"}}],"requestLimitInfo":{"isUnlimited":true}}}}}`))
						default:
							t.Errorf("unexpected local mock operation %q", r.URL.Query().Get("op"))
							http.Error(w, "unexpected operation", http.StatusBadRequest)
						}
					default:
						t.Errorf("unexpected local mock path %q", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer auth.Close()
				cfg := &config.Config{
					WarpBaseURL: auth.URL, WarpFirebaseBaseURL: auth.URL, WarpTokenBaseURL: auth.URL,
					WarpFirebaseAPIKey: projectKey,
				}
				a := New(s, "", "", cfg)
				challenge, err := flow.Start(t.Context(), cfg)
				testutil.NoError(t, err, "start registered Warp login: %v")
				const loginID = "warp-integration-login"
				registry := newBrowserLoginRegistry(channel.Warp)
				testutil.True(t, registry.admit(loginID, &browserLogin{
					deviceLogin: deviceLogin{
						deviceCode: challenge.DeviceCode, userCode: challenge.UserCode,
						verifyURI: challenge.VerifyURI, verifyFull: challenge.VerifyFull,
						expiresAt: challenge.ExpiresAt, status: "pending", configSnapshot: cfg,
						// A fresh login defaults to enabled; re-login must not override
						// the existing disabled row even when this intent is explicit.
						enabled: true, enabledKnown: true,
					},
					challenge: challenge, flow: flow,
				}), "admit generic login transaction")
				pending := httptest.NewRecorder()
				respondLoginStatus(pending, registry, loginID, "Warp")
				testutil.Equal(t, pending.Code, http.StatusOK)
				assertWarpLoginResponseIsSafe(t, pending.Body.Bytes(), deviceCode, customToken, accessToken, refreshToken, projectKey, quotaPrivate, quotaError)

				if completion == "finishBrowserLogin" {
					grant, err := flow.Exchange(t.Context(), cfg, challenge)
					testutil.NoError(t, err, "exchange registered Warp challenge: %v")
					fresh, err := flow.Build(t.Context(), cfg, loginID, challenge, grant)
					testutil.NoError(t, err, "build registered Warp account: %v")
					testutil.Equal(t, fresh.UserID, existing.UserID)
					testutil.NotEqual(t, fresh.WarpRefreshToken, existing.WarpRefreshToken)
					testutil.NotEqual(t, fresh.Name, existing.Name)
					testutil.True(t, fresh.Enabled, "fixture must conflict with operator's disabled setting")
					finishBrowserLogin(a, t.Context(), registry, loginID, "Warp", fresh, flow.MarkReplace, flow.Merge)
				} else {
					// Synchronous polling with a zero interval covers the generic
					// forwarding of both registered Merge and MarkReplace callbacks.
					a.pollChannelLogin(t.Context(), loginID, "Warp", registry)
				}

				final := httptest.NewRecorder()
				respondLoginStatus(final, registry, loginID, "Warp")
				testutil.Equal(t, final.Code, http.StatusOK)
				assertWarpLoginResponseIsSafe(t, final.Body.Bytes(), deviceCode, customToken, accessToken, refreshToken, projectKey, quotaPrivate, quotaError)
				var result deviceLoginResponse
				testutil.NoError(t, json.Unmarshal(final.Body.Bytes(), &result), "decode final status: %v")
				testutil.Equal(t, result.Status, "complete")
				testutil.Equal(t, result.AccountID, existing.ID)
				testutil.Equal(t, result.Message, "Warp account credentials refreshed")
				testutil.Equal(t, result.UserCode, "")
				testutil.Equal(t, result.VerificationURIComplete, "")

				rows, err := s.ListAccounts(t.Context())
				testutil.NoError(t, err, "list accounts after re-login: %v")
				testutil.Equal(t, len(rows), 1)
				stored, err := s.GetAccount(t.Context(), existing.ID)
				testutil.NoError(t, err, "read refreshed account: %v")
				testutil.Equal(t, stored.ID, before.ID)
				testutil.Equal(t, stored.AccountType, "warp")
				testutil.Equal(t, stored.UserID, before.UserID)
				testutil.False(t, stored.Enabled, "re-login re-enabled an operator-disabled row")
				testutil.Equal(t, stored.Name, before.Name)
				testutil.Equal(t, stored.Weight, before.Weight)
				testutil.Equal(t, stored.MaxConcurrent, before.MaxConcurrent)
				testutil.Equal(t, stored.StatusCode, "")
				testutil.Equal(t, stored.AuthStatus, store.AccountAuthStatusActive)
				testutil.Equal(t, stored.StatusMessage, "")
				testutil.True(t, stored.LastAttempt.IsZero(), "old credential auth attempt survived re-login")
				testutil.True(t, stored.QuotaResetAt.IsZero(), "old credential cooldown survived re-login")
				testutil.Equal(t, stored.RequestCount, before.RequestCount)
				testutil.Equal(t, stored.UsageTotal, before.UsageTotal)
				testutil.Equal(t, stored.TokensToday, before.TokensToday)
				testutil.Equal(t, stored.TokensDate, before.TokensDate)
				testutil.Equal(t, stored.UsageCurrent, before.UsageCurrent)
				testutil.Equal(t, stored.UsageLimit, before.UsageLimit)
				testutil.True(t, stored.LastUsedAt.Equal(before.LastUsedAt), "re-login changed last-use state")
				testutil.True(t, stored.CreatedAt.Equal(before.CreatedAt), "re-login changed creation time")
				testutil.Equal(t, stored.Email, "fresh@example.invalid")
				testutil.Equal(t, stored.WarpAccessToken, accessToken)
				testutil.Equal(t, stored.WarpRefreshToken, refreshToken)
				testutil.True(t, stored.WarpExpiresAt.After(now), "new credential expiry was not persisted")
				testutil.True(t, slices.Equal(stored.WarpModelIDs, []string{"new-model-a", "new-model-b"}), "fresh model snapshot was not persisted")
				testutil.True(t, stored.WarpModelsSyncedAt.After(before.WarpModelsSyncedAt), "catalog timestamp did not advance")
				testutil.True(t, stored.VerifiedAt.IsZero(), "old credential verification survived replacement")
				testutil.Equal(t, stored.Token, "")
				testutil.Equal(t, stored.RefreshToken, "")
				testutil.Equal(t, stored.ClientCookie, "")
				testutil.False(t, stored.WarpQuota.AttemptedAt.IsZero(), "quota attempt was not persisted with the new login")
				if quotaFailure {
					testutil.Equal(t, stored.WarpQuota.Status, "error")
					testutil.True(t, stored.WarpQuota.SyncedAt.IsZero(), "failed quota read invented a successful sync")
					testutil.Equal(t, len(stored.WarpQuota.Windows), 0)
				} else {
					testutil.Equal(t, stored.WarpQuota.Status, "ok")
					testutil.True(t, stored.WarpQuota.SyncedAt.Equal(stored.WarpQuota.AttemptedAt), "successful quota read lost its sync timestamp")
					testutil.Equal(t, len(stored.WarpQuota.Windows), 2)
					workspace, personal := stored.WarpQuota.Windows[0], stored.WarpQuota.Windows[1]
					testutil.Equal(t, workspace.Scope, "workspace_monthly")
					testutil.Equal(t, workspace.Unit, "credits")
					if workspace.Limit == nil || workspace.Used == nil || workspace.Remaining == nil {
						t.Fatal("quota snapshot lost measured workspace credits")
					}
					testutil.Equal(t, *workspace.Limit, float64(100))
					testutil.Equal(t, *workspace.Used, float64(25))
					testutil.Equal(t, *workspace.Remaining, float64(75))
					testutil.Equal(t, personal.Scope, "personal")
					testutil.Equal(t, personal.Unit, "credits")
					testutil.True(t, personal.HasUnlimited && personal.Unlimited, "explicit personal unlimited state was lost")
					testutil.True(t, personal.Limit == nil && personal.Used == nil && personal.Remaining == nil && personal.BonusRemaining == nil, "unlimited personal window invented measurements")
				}
				// Login status stays minimal, while the authenticated account projection
				// may expose only the measured pools, not upstream identity or credentials.
				projected, err := json.Marshal(normalizeAccountOutput(stored))
				testutil.NoError(t, err, "marshal safe account projection: %v")
				for _, private := range []string{deviceCode, customToken, accessToken, refreshToken, projectKey, quotaPrivate, quotaError} {
					testutil.MustNotContain(t, string(projected), private)
				}
				var projection struct {
					Quota     store.WarpQuotaSnapshot `json:"warp_quota"`
					Supported bool                    `json:"quota_supported"`
					Status    string                  `json:"quota_status"`
				}
				testutil.NoError(t, json.Unmarshal(projected, &projection), "decode account quota projection: %v")
				testutil.Equal(t, projection.Status, stored.WarpQuota.Status)
				testutil.Equal(t, projection.Quota.Status, stored.WarpQuota.Status)
				testutil.True(t, reflect.DeepEqual(projection.Quota, stored.WarpQuota), "safe account projection changed observed quota windows")
				testutil.Equal(t, projection.Supported, !quotaFailure)
				testutil.Equal(t, starts.Load(), int64(1))
				testutil.Equal(t, exchanges.Load(), int64(1))
				testutil.Equal(t, firebase.Load(), int64(1))
				testutil.Equal(t, catalogs.Load(), int64(1))
				testutil.Equal(t, quotas.Load(), int64(1))
			})
		}
	}
}

func TestWarpLoginIntegration_MissingProjectConfigRejectsBeforeUpstream(t *testing.T) {
	registeredWarpLoginForTest(t)
	t.Setenv("ORCHIDS_WARP_FIREBASE_API_KEY", "")
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "synthetic upstream detail must not reach browser", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	s, _ := newTestStore(t, "warp-login-missing-config:")
	a := New(s, "", "", &config.Config{
		WarpBaseURL: upstream.URL, WarpFirebaseBaseURL: upstream.URL, WarpTokenBaseURL: upstream.URL,
	})
	rec := httptest.NewRecorder()
	a.handleBrowserLogin(rec, channelLoginRequest(t, http.MethodPost, "/api/warp/login", `{"enabled":true}`), channel.Warp, a.warpLogins)
	testutil.Equal(t, rec.Code, http.StatusBadGateway)
	testutil.Equal(t, rec.Header().Get("Cache-Control"), "no-store")
	assertWarpLoginResponseIsSafe(t, rec.Body.Bytes(), upstream.URL, "synthetic upstream detail")
	var response map[string]string
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response), "decode safe login error: %v")
	testutil.Equal(t, response["code"], "upstream_rejected")
	testutil.Equal(t, response["error"], "Warp refused to start a login transaction")
	testutil.Equal(t, len(response), 2)
	testutil.False(t, strings.Contains(rec.Body.String(), "firebase"), "response exposed private project configuration")
	testutil.Equal(t, calls.Load(), int64(0))
	rows, err := s.ListAccounts(t.Context())
	testutil.NoError(t, err, "list accounts after rejected login: %v")
	testutil.Equal(t, len(rows), 0)
	testutil.Equal(t, len(a.warpLogins.logins), 0)
}
