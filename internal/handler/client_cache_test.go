package handler

import (
	"context"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

type testCachedClient struct {
	id int
}

func (c *testCachedClient) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	return nil
}

func TestGetOrCreateAccountClient_ReusesClientAcrossStatsOnlyAccountUpdates(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{RequestTimeout: 30}
	h := &Handler{
		config:      cfg,
		clientCache: newAccountClientCache(),
	}

	created := 0
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		created++
		return &testCachedClient{id: created}
	})

	base := &store.Account{
		ID:           6,
		AccountType:  "workbuddy",
		ClientCookie: "session-a",
		UpdatedAt:    time.Unix(100, 0),
	}

	first := h.getOrCreateAccountClient(base)
	testutil.False(t, first == nil, "expected first client")
	testutil.Equal(t, created, 1)

	statsOnly := *base
	statsOnly.UpdatedAt = base.UpdatedAt.Add(5 * time.Minute)
	statsOnly.LastUsedAt = time.Unix(200, 0)
	statsOnly.RequestCount = 99
	statsOnly.UsageTotal = 12345
	statsOnly.UsageCurrent = 678

	second := h.getOrCreateAccountClient(&statsOnly)
	testutil.False(t, second == nil, "expected second client")
	testutil.Equal(t, second, first)
	testutil.Equal(t, created, 1)
}

func TestGetOrCreateAccountClient_RebuildsWhenCredentialsChange(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{RequestTimeout: 30}
	h := &Handler{
		config:      cfg,
		clientCache: newAccountClientCache(),
	}

	created := 0
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		created++
		return &testCachedClient{id: created}
	})

	base := &store.Account{
		ID:           6,
		AccountType:  "workbuddy",
		ClientCookie: "session-a",
	}

	first := h.getOrCreateAccountClient(base)
	testutil.False(t, first == nil, "expected first client")

	changed := *base
	changed.ClientCookie = "session-b"

	second := h.getOrCreateAccountClient(&changed)
	testutil.False(t, second == nil, "expected second client")
	testutil.NotEqual(t, second, first)
	testutil.Equal(t, created, 2)
}

// Config saves replace the handler snapshot. Runtime-only controls must not
// evict an existing provider client, even when an account event is delivered.
func TestGetOrCreateAccountClient_ConfigSaveOnlyRebuildsForClientInputs(t *testing.T) {
	t.Parallel()

	base := &store.Account{ID: 16, AccountType: "workbuddy", ClientCookie: "session-a"}
	cfg := &config.Config{WorkBuddyBaseURL: "https://wb-a.example", RequestTimeout: 60}
	h := &Handler{config: cfg, clientCache: newAccountClientCache()}
	h.clientCache.SetConfig(cfg)
	h.clientCache.SetAccountResolver(func(id int64) *store.Account { return base })
	created := 0
	h.SetClientFactory(func(_ *store.Account, _ *config.Config) UpstreamClient {
		created++
		return &testCachedClient{id: created}
	})
	first := h.getOrCreateAccountClient(base)
	testutil.Falsef(t, first == nil || created != 1, "initial client=%v, builds=%d", first, created)

	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"auto refresh token", func(c *config.Config) { c.AutoRefreshToken = true }},
		{"debug enabled", func(c *config.Config) { c.DebugEnabled = true }},
		{"debug SSE", func(c *config.Config) { c.DebugLogSSE = true }},
		{"suppress thinking", func(c *config.Config) { c.SuppressThinking = true }},
		{"max retries", func(c *config.Config) { c.MaxRetries = 5 }},
		{"retry delay", func(c *config.Config) { c.RetryDelay = 1234 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := *h.configSnapshot()
			tc.mutate(&next)
			h.SetConfig(&next)
			h.AccountChanges([]int64{base.ID})
			got := h.getOrCreateAccountClient(base)
			testutil.Falsef(t, got != first || created != 1, "runtime-only config rebuilt client: got=%v, first=%v, builds=%d", got, first, created)
		})
	}

	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"request timeout", func(c *config.Config) { c.RequestTimeout = 120 }},
		{"workbuddy base URL", func(c *config.Config) { c.WorkBuddyBaseURL = "https://wb-b.example" }},
		{"proxy", func(c *config.Config) { c.ProxyHTTPS = "http://proxy.example" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := h.getOrCreateAccountClient(base)
			next := *h.configSnapshot()
			tc.mutate(&next)
			h.SetConfig(&next)
			h.AccountChanges([]int64{base.ID})
			got := h.getOrCreateAccountClient(base)
			testutil.Falsef(t, got == previous || created < 2, "client input change did not rebuild: got=%v, previous=%v, builds=%d", got, previous, created)
		})
	}
}

func TestAccountClientFingerprintCoversProviderConstructionInputs(t *testing.T) {
	t.Parallel()

	base := &store.Account{
		ID:                    9,
		AccountType:           "qoder",
		WorkBuddyAccessToken:  "wb-access",
		WorkBuddyRefreshToken: "wb-refresh",
		WorkBuddyUID:          "wb-user",
		WorkBuddyExpiresAt:    time.Unix(10, 0),
		WorkBuddyModelIDs:     []string{`{"id":"wb-model"}`},
		QoderAccessToken:      "q-access",
		QoderRefreshToken:     "q-refresh",
		QoderExpiresAt:        time.Unix(20, 0),
		QoderMachineID:        "machine-a",
		QoderUserID:           "user-a",
		QoderUserName:         "name-a",
		QoderOrganizationID:   "org-a",
		QoderOrganizationTags: []string{"tag-a"},
		QoderDataPolicy:       true,
		QoderRuntimeInfo:      "runtime-a",
		QoderRuntimeKey:       "key-a",
		QoderModelIDs:         []string{"q-model-a"},
	}
	cfg := &config.Config{
		WorkBuddyBaseURL:    "https://wb-a.example",
		QoderOAuthBaseURL:   "https://oauth-a.example",
		QoderOpenAPIBaseURL: "https://open-a.example",
		QoderInferenceURL:   "https://infer-a.example",
		QoderClientID:       "client-a",
		QoderClientVersion:  "version-a",
	}
	want := accountClientFingerprint(base, cfg)

	accountCases := []struct {
		name   string
		mutate func(*store.Account)
	}{
		{"workbuddy endpoint credential", func(a *store.Account) { a.WorkBuddyRefreshToken = "wb-refresh-b" }},
		{"workbuddy expiry", func(a *store.Account) { a.WorkBuddyExpiresAt = time.Unix(11, 0) }},
		{"workbuddy models", func(a *store.Account) { a.WorkBuddyModelIDs = []string{"wb-model-b"} }},
		{"qoder access", func(a *store.Account) { a.QoderAccessToken = "q-access-b" }},
		{"qoder machine", func(a *store.Account) { a.QoderMachineID = "machine-b" }},
		{"qoder identity", func(a *store.Account) { a.QoderUserID = "user-b" }},
		{"qoder organization", func(a *store.Account) { a.QoderOrganizationTags = []string{"tag-b"} }},
		{"qoder policy", func(a *store.Account) { a.QoderDataPolicy = false }},
		{"qoder models", func(a *store.Account) { a.QoderModelIDs = []string{"q-model-b"} }},
	}
	for _, tc := range accountCases {
		t.Run(tc.name, func(t *testing.T) {
			changed := *base
			tc.mutate(&changed)
			testutil.NotEqual(t, accountClientFingerprint(&changed, cfg), want)
		})
	}

	for _, tc := range []struct {
		name   string
		mutate func(*store.Account)
	}{
		{"derived runtime ciphertext", func(a *store.Account) { a.QoderRuntimeInfo = "runtime-b" }},
		{"derived runtime key", func(a *store.Account) { a.QoderRuntimeKey = "key-b" }},
		{"catalog observation time", func(a *store.Account) { a.QoderModelsSyncedAt = time.Now() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := *base
			tc.mutate(&changed)
			testutil.Equal(t, accountClientFingerprint(&changed, cfg), want)
		})
	}

	configCases := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"workbuddy base URL", func(c *config.Config) { c.WorkBuddyBaseURL = "https://wb-b.example" }},
		{"qoder OAuth URL", func(c *config.Config) { c.QoderOAuthBaseURL = "https://oauth-b.example" }},
		{"qoder OpenAPI URL", func(c *config.Config) { c.QoderOpenAPIBaseURL = "https://open-b.example" }},
		{"qoder inference URL", func(c *config.Config) { c.QoderInferenceURL = "https://infer-b.example" }},
		{"qoder client id", func(c *config.Config) { c.QoderClientID = "client-b" }},
		{"qoder client version", func(c *config.Config) { c.QoderClientVersion = "version-b" }},
		{"request timeout", func(c *config.Config) { c.RequestTimeout = 90 }},
		{"proxy", func(c *config.Config) { c.ProxyHTTPS = "https://proxy.example" }},
	}
	for _, tc := range configCases {
		t.Run(tc.name, func(t *testing.T) {
			changed := *cfg
			tc.mutate(&changed)
			testutil.NotEqual(t, accountClientFingerprint(base, &changed), want)
		})
	}
}
