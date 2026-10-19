package qoder

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

func init() { provider.Register(spec()) }

// spec describes the Qoder channel to the rest of the gateway.
func spec() provider.Spec {
	return provider.Spec{
		ID:                   channel.Qoder,
		NewClient:            newProviderClient,
		Capabilities:         capabilities{},
		ConversationAffinity: true,
		// Whether business code 10605 gates the whole upstream or one account
		// has not been established. Probing a few idle accounts first serves the
		// per-account case and costs one quick refusal each in the shared case.
		QueueRefusalAccountProbes: 3,
		Probe: &provider.Probe{
			Subject:  "Qoder control plane",
			Endpoint: DefaultOpenAPIBaseURL,
			Run: func(ctx context.Context, cfg *config.Config) error {
				client := NewFromAccount(nil, cfg)
				defer client.Close()
				return client.ProbeReachability(ctx)
			},
		},
		Catalog:  catalogPort(),
		Admin:    adminPort(),
		Maintain: maintain,
	}
}

// newClientWithStore builds a client that writes a rotated credential back.
// The upstream rotates the device refresh token on every refresh, so a client
// without the store would keep using a credential the pool has already replaced.
func newClientWithStore(acc *store.Account, cfg *config.Config, s *store.Store) *Client {
	client := NewFromAccount(acc, cfg)
	if s != nil {
		client.SetAccountStore(s)
	}
	return client
}

func newProviderClient(acc *store.Account, cfg *config.Config, s *store.Store) provider.Client {
	return newClientWithStore(acc, cfg, s)
}

type capabilities struct{}

// SupportsModel uses the same observed catalog required by the request builder.
// Accounts without a usable snapshot cannot serve inference yet.
func (capabilities) SupportsModel(acc *store.Account, model string) bool {
	if acc == nil || len(acc.QoderModelIDs) == 0 {
		return false
	}
	_, err := catalogFromIDs(acc.QoderModelIDs).Resolve(model)
	return err == nil
}

func (capabilities) IsFreeModel(acc *store.Account, model string) bool {
	if acc == nil {
		return false
	}
	return IsFreeModel(acc.QoderModelIDs, model)
}

func (capabilities) ContextWindows(acc *store.Account) (map[string]int, map[string]int) {
	if acc == nil {
		return nil, nil
	}
	return CatalogContextWindows(acc.QoderModelIDs), CatalogOutputWindows(acc.QoderModelIDs)
}

func (capabilities) HonorsModelCooldown() bool { return true }

// billingSource names where a model's tier came from in the discovery report.
const billingSource = "qoder_price_factor"

func catalogPort() *provider.Catalog {
	return &provider.Catalog{
		Source:                "qoder_upstream_models",
		RequestTimeoutSeconds: 15,
		Observe: func(ctx context.Context, env provider.Env, acc *store.Account) (provider.Observation, error) {
			client := newClientWithStore(acc, env.Config, env.Store)
			defer client.Close()
			catalog, err := client.FetchUpstreamModels(ctx)
			return provider.Observation{
				Models: catalogModels(catalog),
				Record: provider.RecordSnapshot(CatalogSnapshot(catalog), setCatalogSnapshot),
			}, err
		},
		LastKnown: func(acc *store.Account) []provider.Model {
			if acc == nil {
				return nil
			}
			rows := provider.ParseSnapshotRows(acc.QoderModelIDs)
			out := make([]provider.Model, 0, len(rows))
			for _, row := range rows {
				name := strings.TrimSpace(row.Name)
				displayName := strings.TrimSpace(row.DisplayName)
				id := strings.ToLower(util.FirstNonEmpty(name, displayName, strings.TrimSpace(row.Key), strings.TrimSpace(row.ID)))
				if id == "" {
					continue
				}
				model := provider.Model{ID: id, Name: util.FirstNonEmpty(name, displayName, id)}
				model.BillingTier, model.BillingSource = billingTier(row.PriceFactor)
				out = append(out, model)
			}
			return out
		},
		SetSnapshot: setCatalogSnapshot,
	}
}

func setCatalogSnapshot(acc *store.Account, rows []string, now time.Time) {
	acc.QoderModelIDs, acc.QoderModelsSyncedAt = rows, now
}

// billingTier reads the tier a price factor states. A missing factor is
// unknown, not free.
func billingTier(priceFactor *float64) (tier, source string) {
	switch {
	case priceFactor == nil:
		return "", ""
	case *priceFactor == 0:
		return "free", billingSource
	default:
		return "metered", billingSource
	}
}

// catalogModels maps the account catalog onto the channel's public model
// records.
//
// The public identifier is the display name, not the internal gateway key: the
// channel resolves either form, and a client that saw "Qwen3.7-Max" in
// /v1/models must be able to ask for it by that name. The internal key stays in
// the account snapshot, which is what the client resolves against.
func catalogModels(catalog *Catalog) []provider.Model {
	entries := catalog.Entries()
	out := make([]provider.Model, 0, len(entries))
	for _, entry := range entries {
		key := strings.TrimSpace(entry.Key)
		if key == "" {
			continue
		}
		// Model lookup is lowercased before it reaches the store index, so the
		// public identifier is stored in lowercase. The upstream key and the
		// display name are both still accepted at request time, because the
		// catalog resolves case-insensitively.
		id := strings.ToLower(util.FirstNonEmpty(strings.TrimSpace(entry.Name), key))
		model := provider.Model{ID: id, Name: id}
		model.BillingTier, model.BillingSource = billingTier(entry.PriceFactor)
		out = append(out, model)
	}
	return out
}

// maintain keeps one account's credential, catalog and allowance current.
func maintain(ctx context.Context, env provider.Env, acc *store.Account) {
	refreshCredential(ctx, env, acc)
	refreshCatalog(ctx, env, acc)
	refreshQuota(ctx, env, acc)
}

// credentialRefreshLead is how far ahead of expiry the scheduler renews an
// access token. The request path only renews inside RefreshLead, so without
// this the request that meets an expiring token waits for the OAuth round trip
// and fails with it.
const credentialRefreshLead = 5 * time.Minute

func credentialRefreshDue(acc *store.Account, now time.Time) bool {
	if acc == nil || !store.AccountAuthActive(acc) {
		return false
	}
	creds := ResolveCredentials(acc)
	if strings.TrimSpace(creds.RefreshToken) == "" || creds.AccessExpiresAt.IsZero() {
		return false
	}
	if !creds.RefreshExpiresAt.IsZero() && now.After(creds.RefreshExpiresAt) {
		// Only a new login helps; the request path reports that verdict.
		return false
	}
	return creds.AccessExpiresAt.Sub(now) <= credentialRefreshLead
}

// refreshCredential renews an access token that is about to expire. The
// rotation goes through the client's refresh path, which serializes with
// request-time refreshes and persists under the expected refresh token, so a
// request that renewed first is not renewed again.
func refreshCredential(ctx context.Context, env provider.Env, acc *store.Account) {
	if env.Store == nil || !credentialRefreshDue(acc, time.Now()) {
		return
	}
	client := newClientWithStore(acc, env.Config, env.Store)
	defer client.Close()
	refreshCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err := client.RefreshAhead(refreshCtx)
	cancel()
	if err != nil {
		slog.Warn("Auto refresh qoder credential failed; the request path will retry", "account_id", acc.ID, "error", err)
		return
	}
	// Later maintenance steps build their clients from acc; hand them the
	// rotated credential instead of the one just consumed.
	client.CopyAccountState(acc)
}

func catalogRefreshDue(acc *store.Account, now time.Time) bool {
	if acc == nil || len(acc.QoderModelIDs) == 0 || acc.QoderModelsSyncedAt.IsZero() {
		return true
	}
	return now.Sub(acc.QoderModelsSyncedAt) >= provider.HealthRefreshInterval
}

func refreshCatalog(ctx context.Context, env provider.Env, acc *store.Account) {
	if acc == nil || env.Store == nil || !catalogRefreshDue(acc, time.Now()) {
		return
	}
	client := newClientWithStore(acc, env.Config, env.Store)
	defer client.Close()
	catalogCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	catalog, err := client.FetchUpstreamModels(catalogCtx)
	cancel()
	if err != nil {
		slog.Warn("Auto refresh qoder catalog failed; keeping the last snapshot", "account_id", acc.ID, "error", err)
		return
	}
	provider.PersistSnapshot(ctx, env.Store, acc, catalogPort(), CatalogSnapshot(catalog), "Auto refresh qoder catalog: update account failed")
}

func quotaRefreshDue(acc *store.Account, now time.Time) bool {
	if acc == nil || acc.QoderQuota.SyncedAt.IsZero() {
		return true
	}
	return now.Sub(acc.QoderQuota.SyncedAt) >= provider.HealthRefreshInterval
}

// refreshQuota reads the same authoritative allowance endpoint used by a
// manual account refresh. Inference-agent limit payloads are intentionally not
// used here: they can be model-scoped while the account still has credits.
func refreshQuota(ctx context.Context, env provider.Env, acc *store.Account) {
	if acc == nil || env.Store == nil || !quotaRefreshDue(acc, time.Now()) {
		return
	}
	if !refreshqueue.WithLease(acc.ID, func() {
		client := newClientWithStore(acc, env.Config, env.Store)
		defer client.Close()
		quotaCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		quota, err := client.FetchQuota(quotaCtx)
		cancel()
		if err != nil {
			slog.Warn("Auto refresh qoder quota failed; keeping the last account verdict", "account_id", acc.ID, "error", err)
			return
		}
		ApplyQuota(acc, quota)
		if quota.Exhausted {
			accountpolicy.Verdict{
				Status:  store.AccountStatusQoderQuotaExhausted,
				Message: "Qoder allowance exhausted; free catalog models remain eligible",
				Scope:   accountpolicy.ScopeAccount,
				At:      time.Now(),
			}.Apply(acc)
		} else {
			accountpolicy.Success(time.Now()).Apply(acc)
		}
		if err := env.Store.UpdateAccount(ctx, acc); err != nil {
			slog.Warn("Auto refresh qoder quota: update account failed", "account_id", acc.ID, "error", err)
		}
	}) {
		slog.Debug("Auto refresh qoder quota: account already refreshing", "account_id", acc.ID)
	}
}
