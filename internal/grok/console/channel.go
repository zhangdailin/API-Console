package console

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/grok/web"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// ProviderConsole is the Console plane's provider id.
const ProviderConsole = store.GrokProviderConsole

// RoutePrefix qualifies every Console route.
const RoutePrefix = store.GrokConsoleRoutePrefix

// ErrMissingSSOToken is returned for a Console account without a cookie.
var ErrMissingSSOToken = errors.New("missing sso token")

const (
	maintenanceInterval = 30 * time.Minute
	maintenanceRetry    = 10 * time.Minute
	// observationTTL bounds how old a catalog read may be and still publish
	// the account's routes.
	observationTTL = 24 * time.Hour
)

var maintenanceAttempts sync.Map // account ID -> time.Time

// IsConsoleAccount reports whether an account belongs to the Console plane.
func IsConsoleAccount(acc *store.Account) bool { return acc.IsGrokConsole() }

// IsConsoleModelID reports whether a public model id names a Console route.
func IsConsoleModelID(modelID string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(modelID)), RoutePrefix)
}

// UpstreamModel strips the Console qualifier from a public model id.
func UpstreamModel(modelID string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(modelID)), RoutePrefix)
}

// RouteID is the public id of an upstream Console model.
func RouteID(upstreamModel string) string {
	return RoutePrefix + strings.ToLower(strings.TrimSpace(upstreamModel))
}

// SSOToken returns the normalized SSO cookie a Console account carries.
func SSOToken(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	return web.NormalizeSSOToken(util.FirstNonEmpty(acc.ClientCookie, acc.RefreshToken, acc.Token))
}

// LooksLikeConsoleAccount reports an account submitted for the Console plane.
func LooksLikeConsoleAccount(acc *store.Account) bool {
	return acc != nil && strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") &&
		strings.EqualFold(strings.TrimSpace(acc.GrokProvider), ProviderConsole)
}

// PrepareAccount normalizes a Console account before it is written.
func PrepareAccount(acc *store.Account) error {
	if acc == nil {
		return ErrMissingSSOToken
	}
	token := SSOToken(acc)
	acc.CredentialType = "sso"
	acc.GrokProvider = ProviderConsole
	acc.OAuthAccessToken, acc.OAuthRefreshToken, acc.OAuthExpiresAt = "", "", time.Time{}
	acc.Token, acc.RefreshToken = "", ""
	acc.ClientCookie = token
	if token == "" {
		return ErrMissingSSOToken
	}
	if acc.Weight <= 0 {
		acc.Weight = 1
	}
	return nil
}

// PrepareUpdate keeps the stored cookie when an edit sends none (secrets are
// redacted on read) and resets the observations of a replaced cookie.
func PrepareUpdate(acc, existing *store.Account) error {
	if acc == nil || existing == nil {
		return PrepareAccount(acc)
	}
	if SSOToken(acc) == "" {
		acc.ClientCookie = existing.ClientCookie
	}
	if SSOToken(acc) != SSOToken(existing) {
		acc.ClearVerifiedAt = true
		acc.StatusCode, acc.StatusMessage = "", ""
	}
	return PrepareAccount(acc)
}

// CredentialKey identifies the credential. It differs from the Web key on
// purpose: one cookie may back a Web and a Console account at once.
func CredentialKey(acc *store.Account) string {
	if token := SSOToken(acc); token != "" {
		return "grok:console:" + token
	}
	return ""
}

// IdentityKey is the Console user behind an account, once known.
func IdentityKey(acc *store.Account) string {
	if acc != nil && strings.TrimSpace(acc.UserID) != "" {
		return "grok:console:user:" + strings.TrimSpace(acc.UserID)
	}
	return ""
}

// Fingerprint is a non-reversible digest of the credential.
func Fingerprint(acc *store.Account) string { return util.Fingerprint(SSOToken(acc)) }

// ProjectOutput shapes the copy of an account a response is built from.
func ProjectOutput(out *store.Account) {
	if out == nil {
		return
	}
	out.GrokProvider = ProviderConsole
	out.RefreshToken, out.OAuthAccessToken, out.OAuthRefreshToken = "", "", ""
}

// ProjectQuota reports a Console account's weekly included-usage pool, in US
// dollars, as the grok.com dashboard last reported it.
func ProjectQuota(fields map[string]any, acc *store.Account) {
	if acc == nil {
		return
	}
	fields["plan_name"] = strings.TrimSpace(acc.Subscription)
	fields["quota_unit"] = "usd"
	fields["quota_mode"] = "unknown"
	fields["quota_supported"] = false
	quota := acc.GrokConsoleQuota
	switch {
	case quota.SyncedAt.IsZero():
		fields["quota_note"] = "尚未读取 Console 额度"
	case quota.Unlinked:
		fields["quota_note"] = "该 Grok 账号未与 Cursor 关联，没有 Console 额度"
		fields["quota_synced_at"] = quota.SyncedAt
	default:
		fields["quota_mode"] = "console_weekly"
		fields["quota_supported"] = true
		fields["quota_limit"] = float64(quota.BudgetCents) / 100
		fields["quota_used"] = float64(quota.UsedCents) / 100
		fields["quota_remaining"] = float64(max(0, quota.BudgetCents-quota.UsedCents)) / 100
		fields["quota_synced_at"] = quota.SyncedAt
		if !quota.ResetAt.IsZero() {
			fields["quota_reset_at"] = quota.ResetAt
		}
	}
	fields["console_models"] = len(acc.GrokConsoleModels)
	if !acc.GrokConsoleModelsSyncedAt.IsZero() {
		fields["console_models_synced_at"] = acc.GrokConsoleModelsSyncedAt
	}
}

// VerifyAccount proves the cookie against console.x.ai and records the
// account's model catalog. It returns "401" when the session is refused.
func VerifyAccount(ctx context.Context, cfg *config.Config, acc *store.Account) (string, error) {
	return verifyWith(ctx, NewClient(cfg), acc)
}

func verifyWith(ctx context.Context, client *Client, acc *store.Account) (string, error) {
	token := SSOToken(acc)
	if token == "" {
		return "401", ErrMissingSSOToken
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	models, err := client.FetchModels(ctx, token)
	if errors.Is(err, ErrUnauthorized) {
		return "401", err
	}
	if err != nil {
		return "", err
	}
	acc.GrokConsoleModels = models
	acc.GrokConsoleModelsSyncedAt = time.Now().UTC()
	acc.AuthStatus = store.AccountAuthStatusActive
	readAllowance(ctx, client, acc, token)
	return "", nil
}

// readAllowance records the weekly pool. It never fails verification: the
// dashboard is a separate surface, and its refusal says nothing about the
// credential's inference access.
func readAllowance(ctx context.Context, client *Client, acc *store.Account, token string) {
	allowance, err := client.FetchAllowance(ctx, token)
	now := time.Now()
	switch {
	case err == nil:
		ApplyAllowance(acc, allowance, now)
	case errors.Is(err, ErrUnlinked):
		ApplyUnlinked(acc, now)
	default:
		slog.Warn("grok console allowance read failed", "account_id", acc.ID, "error", redact(err, acc))
	}
}

// Maintain re-reads a Console account on its own cadence and republishes the
// Console routes when the account's catalog changed.
func Maintain(ctx context.Context, cfg *config.Config, st *store.Store, acc *store.Account) {
	maintainWith(ctx, NewClient(cfg), st, acc)
}

func maintainWith(ctx context.Context, client *Client, st *store.Store, acc *store.Account) {
	if acc == nil || st == nil || !IsConsoleAccount(acc) || !acc.Enabled || !store.AccountAuthActive(acc) {
		return
	}
	// A spent pool is re-read on the recovery queue's schedule (its reset,
	// or bounded backoff) instead of the regular cadence.
	recovering := claimAllowanceRecovery(acc, time.Now())
	if !recovering {
		if !acc.GrokConsoleModelsSyncedAt.IsZero() && time.Since(acc.GrokConsoleModelsSyncedAt) < maintenanceInterval {
			return
		}
		if last, ok := maintenanceAttempts.Load(acc.ID); ok && time.Since(last.(time.Time)) < maintenanceRetry {
			return
		}
	}
	maintenanceAttempts.Store(acc.ID, time.Now())
	before := strings.Join(acc.GrokConsoleModels, ",")
	status, err := verifyWith(ctx, client, acc)
	now := time.Now()
	if recovering {
		settleAllowanceRecovery(acc, now)
	}
	switch {
	case status == "401":
		acc.StatusCode = "401"
		acc.AuthStatus = store.AccountAuthStatusReauthRequired
		acc.StatusMessage = "console.x.ai 已不接受该 SSO Cookie，需要重新导入"
		acc.LastAttempt = now
	case err != nil:
		slog.Warn("grok console account maintenance failed", "account_id", acc.ID, "error", redact(err, acc))
		return
	default:
		if strings.TrimSpace(acc.StatusCode) == "401" {
			acc.StatusCode, acc.StatusMessage = "", ""
		}
		acc.VerifiedAt = now
	}
	if err := st.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("grok console account maintenance: update failed", "account_id", acc.ID, "error", err)
		return
	}
	if strings.Join(acc.GrokConsoleModels, ",") != before || status == "401" {
		if err := PublishRoutes(ctx, st); err != nil {
			slog.Warn("grok console route publication failed", "error", err)
		}
	}
}

// accountObservesRoutes reports whether an account's stored catalog is
// current evidence for its routes.
func accountObservesRoutes(acc *store.Account, now time.Time) bool {
	if !IsConsoleAccount(acc) || !acc.Enabled || !store.AccountAuthActive(acc) || SSOToken(acc) == "" || strings.TrimSpace(acc.StatusCode) == "401" {
		return false
	}
	synced := acc.GrokConsoleModelsSyncedAt
	return !synced.IsZero() && now.Sub(synced) < observationTTL
}

// PublishRoutes reconciles the Console rows with the catalogs active Console
// accounts read within the last 24 hours. No such account: every Console row
// is pruned and nothing is published.
func PublishRoutes(ctx context.Context, st *store.Store) error {
	if st == nil {
		return errors.New("store not configured")
	}
	accounts, err := st.ListAccounts(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	seen := map[string]bool{}
	var rows []*store.Model
	for _, acc := range accounts {
		if !accountObservesRoutes(acc, now) {
			continue
		}
		for _, upstream := range acc.GrokConsoleModels {
			id := RouteID(upstream)
			if seen[id] {
				continue
			}
			seen[id] = true
			row := &store.Model{
				Channel: "grok", ModelID: id, Name: "Console " + upstream,
				Status: store.ModelStatusAvailable, Verified: true, SortOrder: 2000 + len(rows),
				Provider: ProviderConsole, UpstreamModel: upstream,
			}
			store.ApplyGrokConsoleRouteDefaults(row)
			rows = append(rows, row)
		}
	}
	result, err := st.ReconcileDiscoveredModels(ctx, "grok", rows, store.ModelReconcileOptions{Prune: true, ProviderScope: ProviderConsole})
	if err != nil {
		return err
	}
	if result != nil && (result.Added > 0 || result.Deleted > 0) {
		slog.Info("Published Grok Console routes", "added", result.AddedModelIDs, "deleted", result.DeletedModelIDs)
	}
	return nil
}

func redact(err error, acc *store.Account) string {
	text := err.Error()
	if acc != nil {
		for _, secret := range acc.Secrets() {
			if secret = strings.TrimSpace(secret); len(secret) >= 8 {
				text = strings.ReplaceAll(text, secret, "[redacted]")
			}
		}
	}
	return text
}
