package grok

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

func init() { provider.Register(spec()) }

// spec describes the Grok Build channel to the rest of the gateway. It has no
// NewClient and no Capabilities: inference runs through this package's own
// handler, which speaks Responses natively.
func spec() provider.Spec {
	return provider.Spec{
		ID: channel.Grok,
		Catalog: &provider.Catalog{
			Source:    "grok_build_models",
			Kind:      provider.CatalogAccountView,
			Eligible:  catalogEligible,
			Observe:   observeCatalog,
			LastKnown: lastKnownCatalog,
			// Build is one provider plane of the channel: a complete round may
			// prune Build rows, never the rows of another plane.
			ShapeRecord:   shapeModelRecord,
			ProviderScope: ProviderBuild,
		},
		Admin:    adminPort(),
		Maintain: maintain,
	}
}

// catalogEligible reports whether an account can read the Build catalog: a
// Build OAuth account that still holds a token to present.
func catalogEligible(acc *store.Account) bool {
	if acc == nil || !accountpolicy.IsGrokAccount(acc) || !strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") {
		return false
	}
	return strings.TrimSpace(acc.OAuthAccessToken) != "" || strings.TrimSpace(acc.OAuthRefreshToken) != ""
}

// observeCatalog reads the official Build CLI catalog. It is a control-plane
// read: model refresh must never send a completion simply to discover an
// account's capabilities.
func observeCatalog(ctx context.Context, env provider.Env, acc *store.Account) (provider.Observation, error) {
	client := NewCLIClient(env.Config)
	client.SetAccountStore(env.Store)
	catalog, err := client.FetchModelCatalog(ctx, acc)
	return CatalogObservation(catalog), err
}

// CatalogObservation wraps one upstream catalog read. Recording it projects
// the catalog onto the account; the published rows are then read back through
// LastKnown, so a row is always what the account itself now stores.
func CatalogObservation(catalog []modelcatalog.Profile) provider.Observation {
	return provider.Observation{
		Record: func(acc *store.Account, now time.Time) bool {
			if acc == nil || len(catalog) == 0 {
				return false
			}
			NormalizeProvider(acc)
			ApplyCLIModelCatalog(acc, catalog, now)
			return true
		},
	}
}

// shapeModelRecord stamps the Build routing metadata on a discovered row.
func shapeModelRecord(record *store.Model) {
	if record == nil {
		return
	}
	store.ApplyGrokRouteDefaults(record)
	record.Provider, record.UpstreamModel = ProviderBuild, record.ModelID
	if strings.Contains(strings.ToLower(record.ModelID), "video") {
		record.Capabilities = []string{store.CapabilityVideo}
	} else {
		record.Capabilities = []string{store.CapabilityChat, store.CapabilityMessages, store.CapabilityResponses}
	}
}

func lastKnownCatalog(acc *store.Account) []provider.Model {
	if acc == nil {
		return nil
	}
	out := make([]provider.Model, 0, len(acc.GrokModels))
	for _, rawID := range acc.GrokModels {
		id := strings.ToLower(strings.TrimSpace(rawID))
		if id == "" {
			continue
		}
		name := id
		for _, profile := range acc.GrokModelCatalog {
			if strings.EqualFold(profile.ModelID, rawID) && strings.TrimSpace(profile.Name) != "" {
				name = profile.Name
				break
			}
		}
		out = append(out, provider.Model{ID: id, Name: name})
	}
	return out
}

// maintain refreshes one Build OAuth account through its own token lifecycle.
//
// A credential the upstream already refused needs a human, not another
// refresh. Skipping it here is the convergence step: it stops consuming
// refresh cycles and writing the same warning every interval until an operator
// logs in again (which restores the active state).
func maintain(ctx context.Context, env provider.Env, acc *store.Account) {
	if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") {
		return
	}
	if !store.AccountAuthActive(acc) || !acc.Enabled {
		return
	}
	refreshCLIAccount(ctx, env.Config, env.Store, acc)
}

// refreshCLIAccount refreshes a Build CLI OAuth account access token before it
// expires. The CLIClient oauth layer handles the refresh_token grant and
// persists rotated tokens back to Redis.
func refreshCLIAccount(ctx context.Context, cfg *config.Config, s *store.Store, acc *store.Account) {
	if acc == nil || s == nil || cfg == nil {
		return
	}
	// Persist the compatibility inference for pre-provider OAuth rows while we
	// already have a safe background update. This makes the product boundary
	// visible to the admin API instead of leaving old records ambiguous.
	NormalizeProvider(acc)
	cliClient := NewCLIClient(cfg)
	cliClient.SetAccountStore(s)
	if strings.TrimSpace(acc.OAuthAccessToken) == "" || acc.OAuthExpiresAt.IsZero() || time.Until(acc.OAuthExpiresAt) <= 5*time.Minute {
		refreshCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		token, err := cliClient.OAuthAccessToken(refreshCtx, acc)
		cancel()
		if err != nil {
			if IsCLIPermanentOAuthError(err) {
				acc.StatusCode = "401"
				acc.AuthStatus = store.AccountAuthStatusReauthRequired
				acc.StatusMessage = "上游已不接受该 OAuth 授权（refresh token 被拒绝），需要重新登录"
				acc.LastAttempt = time.Now()
				// Distinguish "the upstream invalidated this grant" from "our record
				// was wiped": both surface as 401, but only the former is expected
				// when a second login for the same xAI user rotates/retires the old
				// refresh token.
				slog.Warn("Grok CLI refresh token was rejected by the upstream; this account needs a new login",
					"account_id", acc.ID,
					"account_name", acc.Name,
					"email", acc.Email,
					"team_id", acc.TeamID,
					"has_refresh_token", strings.TrimSpace(acc.OAuthRefreshToken) != "",
					"token_fingerprint", TokenFingerprint(acc.OAuthRefreshToken),
					"error", err)
			} else {
				slog.Warn("Auto refresh grok cli token failed", "account_id", acc.ID, "error", err)
			}
			if updateErr := s.UpdateAccount(ctx, acc); updateErr != nil {
				slog.Warn("Auto refresh grok cli: update account failed", "account_id", acc.ID, "error", updateErr)
			}
			return
		}
		if token != "" {
			acc.OAuthAccessToken = token
			acc.StatusCode = ""
			acc.AuthStatus = store.AccountAuthStatusActive
			acc.LastAttempt = time.Time{}
		}
	}

	now := time.Now()
	result := RefreshBuildAccount(ctx, cliClient, acc, BuildRefreshOptions{
		Billing: cliBillingNeedsSync(acc, now), BillingTimeout: 20 * time.Second,
		Models: CLIModelsNeedSync(acc, now), ModelsTimeout: 15 * time.Second,
		Now: now,
	})
	if result.BillingErr != nil {
		slog.Warn("Auto sync grok cli billing failed; leaving numeric quota unavailable", "account_id", acc.ID, "error", result.BillingErr)
	}
	if result.ModelsErr != nil {
		slog.Warn("Auto sync grok cli model catalog failed; keeping last catalog", "account_id", acc.ID, "error", result.ModelsErr)
	}
	if updateErr := s.UpdateAccount(ctx, acc); updateErr != nil {
		slog.Warn("Auto refresh grok cli: update account failed", "account_id", acc.ID, "error", updateErr)
	}
}

func cliBillingNeedsSync(acc *store.Account, now time.Time) bool {
	if acc == nil || acc.GrokBilling.SyncedAt.IsZero() {
		return true
	}
	return now.Sub(acc.GrokBilling.SyncedAt) >= provider.HealthRefreshInterval
}
