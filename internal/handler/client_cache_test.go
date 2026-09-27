package handler

import (
	"context"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
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
		config:       cfg,
		clientCache:  newAccountClientCache(),
		sessionStore: NewMemorySessionStore(30*time.Minute, 1024),
	}

	created := 0
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		created++
		return &testCachedClient{id: created}
	})

	base := &store.Account{
		ID:            6,
		AccountType:   "workbuddy",
		SessionCookie: "session-a",
		UpdatedAt:     time.Unix(100, 0),
	}

	first := h.getOrCreateAccountClient(base)
	if first == nil {
		t.Fatal("expected first client")
	}
	if created != 1 {
		t.Fatalf("created=%d want 1", created)
	}

	statsOnly := *base
	statsOnly.UpdatedAt = base.UpdatedAt.Add(5 * time.Minute)
	statsOnly.LastUsedAt = time.Unix(200, 0)
	statsOnly.RequestCount = 99
	statsOnly.UsageTotal = 12345
	statsOnly.UsageCurrent = 678

	second := h.getOrCreateAccountClient(&statsOnly)
	if second == nil {
		t.Fatal("expected second client")
	}
	if second != first {
		t.Fatal("expected stats-only update to reuse cached client")
	}
	if created != 1 {
		t.Fatalf("created=%d want 1 after stats-only update", created)
	}
}

func TestGetOrCreateAccountClient_RebuildsWhenCredentialsChange(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{RequestTimeout: 30}
	h := &Handler{
		config:       cfg,
		clientCache:  newAccountClientCache(),
		sessionStore: NewMemorySessionStore(30*time.Minute, 1024),
	}

	created := 0
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		created++
		return &testCachedClient{id: created}
	})

	base := &store.Account{
		ID:            6,
		AccountType:   "workbuddy",
		SessionCookie: "session-a",
	}

	first := h.getOrCreateAccountClient(base)
	if first == nil {
		t.Fatal("expected first client")
	}

	changed := *base
	changed.SessionCookie = "session-b"

	second := h.getOrCreateAccountClient(&changed)
	if second == nil {
		t.Fatal("expected second client")
	}
	if second == first {
		t.Fatal("expected credential change to rebuild cached client")
	}
	if created != 2 {
		t.Fatalf("created=%d want 2 after credential change", created)
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
		WorkBuddyModelIDs:     []string{"wb-model"},
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
			if got := accountClientFingerprint(&changed, cfg); got == want {
				t.Fatal("fingerprint did not change")
			}
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
			if got := accountClientFingerprint(&changed, cfg); got != want {
				t.Fatal("derived Qoder state unexpectedly invalidated its own client")
			}
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
		{"qoder protocol profile", func(c *config.Config) { c.QoderProtocolProfile = "skill-cli" }},
		{"qoder client id", func(c *config.Config) { c.QoderClientID = "client-b" }},
		{"qoder client version", func(c *config.Config) { c.QoderClientVersion = "version-b" }},
	}
	for _, tc := range configCases {
		t.Run(tc.name, func(t *testing.T) {
			changed := *cfg
			tc.mutate(&changed)
			if got := accountClientFingerprint(base, &changed); got == want {
				t.Fatal("fingerprint did not change")
			}
		})
	}
}
