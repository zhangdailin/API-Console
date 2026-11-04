package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// This file is the Web plane's half of the grok channel port: credential
// normalization, verification, scheduled maintenance, quota projection and the
// publication of Web routes. The grok package calls into it for every account
// that IsWebAccount reports.

// ErrMissingSSOToken is returned for a Web account that carries no SSO cookie.
var ErrMissingSSOToken = errors.New("missing sso token")

// webMaintenanceInterval is how often a Web account's identity and quota are
// re-read by the background scheduler.
const webMaintenanceInterval = 30 * time.Minute

// webMaintenanceRetry is the shortest interval between two background probes
// of one account.
const webMaintenanceRetry = 10 * time.Minute

var maintenanceAttempts sync.Map // account ID -> time.Time of the last probe

// webTransportKey summarizes the settings that decide whether grok.com is
// reachable, so a change to them can retry the accounts that failed.
func webTransportKey(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.Join([]string{cfg.ProxyURL, cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.GrokWebProxyURL,
		cfg.GrokConfigCFClearance, cfg.GrokConfigCFBM, cfg.GrokClearanceMode, cfg.GrokFlareSolverrURL, cfg.GrokWebBaseURL, cfg.GrokWebUserAgent}, "\x00")
}

// webObservationTTL bounds how old a successful quota read may be and still
// count as evidence that the account can serve its routes.
const webObservationTTL = 24 * time.Hour

// SSOToken returns the normalized SSO cookie value an account carries.
func SSOToken(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	return NormalizeSSOToken(util.FirstNonEmpty(acc.ClientCookie, acc.RefreshToken, acc.Token))
}

// LooksLikeWebAccount reports whether an account submitted through the admin
// API is meant to be a Web SSO account: it names the Web provider or SSO
// credential mode, or carries a cookie and no OAuth credential.
func LooksLikeWebAccount(acc *store.Account) bool {
	if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(acc.GrokProvider), ProviderWeb) || strings.EqualFold(strings.TrimSpace(acc.CredentialType), "sso") {
		return true
	}
	return strings.TrimSpace(acc.OAuthAccessToken) == "" && strings.TrimSpace(acc.OAuthRefreshToken) == "" &&
		util.FirstNonEmpty(acc.ClientCookie, acc.RefreshToken, acc.Token) != ""
}

// PrepareAccount normalizes a Web account before it is written: the cookie is
// stored as "sso=<token>" in ClientCookie, every other credential field is
// cleared, and the provider/credential markers are pinned.
func PrepareAccount(acc *store.Account) error {
	if acc == nil {
		return ErrMissingSSOToken
	}
	token := SSOToken(acc)
	acc.CredentialType = "sso"
	acc.GrokProvider = ProviderWeb
	acc.OAuthAccessToken = ""
	acc.OAuthRefreshToken = ""
	acc.OAuthExpiresAt = time.Time{}
	acc.Token = ""
	acc.RefreshToken = ""
	if token == "" {
		acc.ClientCookie = ""
		return ErrMissingSSOToken
	}
	acc.ClientCookie = "sso=" + token
	if strings.TrimSpace(acc.Name) == "" {
		acc.Name = "grok-sso-" + util.Fingerprint(token)[:8]
	}
	if acc.Weight <= 0 {
		acc.Weight = 1
	}
	return nil
}

// PrepareUpdate keeps the stored cookie when the admin form submits none (the
// credential is write-only and never read back) and keeps runtime state the
// form does not own.
func PrepareUpdate(acc, existing *store.Account) error {
	if acc == nil || existing == nil {
		return PrepareAccount(acc)
	}
	if SSOToken(acc) == "" {
		acc.ClientCookie = existing.ClientCookie
	}
	if SSOToken(acc) != SSOToken(existing) {
		// A different cookie is a different credential: its observations and
		// verdicts belong to the old one.
		acc.ClearVerifiedAt = true
		acc.GrokWebQuota = store.GrokWebQuotaSnapshot{}
		acc.StatusCode = ""
		acc.StatusMessage = ""
		acc.NSFWEnabled = false
	} else {
		acc.GrokWebQuota = existing.GrokWebQuota
		acc.NSFWEnabled = existing.NSFWEnabled
		acc.StatusCode = existing.StatusCode
		acc.StatusMessage = existing.StatusMessage
		acc.LastAttempt = existing.LastAttempt
		acc.VerifiedAt = existing.VerifiedAt
		acc.QuotaResetAt = existing.QuotaResetAt
	}
	acc.UsageCurrent = existing.UsageCurrent
	acc.UsageTotal = existing.UsageTotal
	acc.UsageLimit = existing.UsageLimit
	acc.QualityRevision = existing.QualityRevision
	acc.QualityFailures = existing.QualityFailures
	acc.QualityCooldownUntil = existing.QualityCooldownUntil
	acc.QualityReason = existing.QualityReason
	if strings.TrimSpace(acc.Subscription) == "" {
		acc.Subscription = existing.Subscription
	}
	return PrepareAccount(acc)
}

// CredentialKey identifies the credential for duplicate detection.
func CredentialKey(acc *store.Account) string {
	if token := SSOToken(acc); token != "" {
		return "grok:sso:" + token
	}
	return ""
}

// IdentityKey identifies the grok.com user behind a Web account.
func IdentityKey(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	if userID := strings.TrimSpace(acc.UserID); userID != "" {
		return "grok:web:user:" + userID
	}
	return ""
}

// Fingerprint is a non-reversible digest of the Web credential.
func Fingerprint(acc *store.Account) string {
	return util.Fingerprint(SSOToken(acc))
}

// ProjectOutput shapes the copy of a Web account a response is built from.
func ProjectOutput(out *store.Account) {
	if out == nil {
		return
	}
	NormalizeProvider(out)
	out.RefreshToken = ""
	out.OAuthAccessToken = ""
	out.OAuthRefreshToken = ""
}

// ProjectQuota publishes the Web auto/fast windows. auto is the headline
// window because it serves every route except the fast ones.
func ProjectQuota(fields map[string]any, acc *store.Account) {
	if acc == nil {
		return
	}
	quota := acc.GrokWebQuota
	tier := webPlanTier(acc)
	fields["plan_name"] = strings.TrimSpace(acc.Subscription)
	fields["plan_tier"] = tier
	fields["grok_pool"] = grokAccountPool(acc)
	fields["nsfw_enabled"] = acc.NSFWEnabled
	fields["quota_unit"] = "requests"
	fields["quota_mode"] = "unknown"
	fields["quota_supported"] = false
	if acc.GrokWebSyncError != "" {
		fields["grok_web_sync_error"] = acc.GrokWebSyncError
	}
	if quota.SyncedAt.IsZero() {
		fields["quota_note"] = "尚未同步 grok.com 额度；点刷新立即同步"
		if acc.GrokWebSyncError != "" {
			fields["quota_note"] = "同步失败：" + acc.GrokWebSyncError
		}
		return
	}
	fields["quota_synced_at"] = quota.SyncedAt
	windows := map[string]store.GrokQuotaWindow{"auto": quota.Auto, "fast": quota.Fast, "weekly": quota.Weekly}
	web := map[string]any{}
	for name, window := range windows {
		if !window.HasLimit && !window.HasRemaining {
			continue
		}
		web[name] = window
	}
	if !quota.Imagine.SyncedAt.IsZero() {
		web["imagine"] = quota.Imagine
	}
	if len(web) > 0 {
		fields["grok_web_quota"] = web
	}
	headline := quota.Auto
	mode := "auto"
	if !headline.HasLimit && !headline.HasRemaining {
		headline, mode = quota.Fast, "fast"
	}
	if headline.HasLimit || headline.HasRemaining {
		fields["quota_mode"] = "web_" + mode
		fields["quota_supported"] = true
		if headline.HasLimit {
			fields["quota_limit"] = headline.Limit
			fields["quota_used"] = max(0, headline.Limit-headline.Remaining)
		}
		fields["quota_remaining"] = max(0, headline.Remaining)
		if !headline.ResetAt.IsZero() {
			fields["quota_reset_at"] = headline.ResetAt
		}
	}
}

func webPlanTier(acc *store.Account) string {
	switch strings.ToLower(strings.TrimSpace(acc.Subscription)) {
	case "", "unknown":
		return "unknown"
	case "basic", "free":
		return "free"
	default:
		return "paid"
	}
}

// VerifyAccount reads the account's identity and quota from grok.com and
// records them on the account in memory. An authentication refusal is the
// only credential verdict; any other failure is an upstream problem.
func VerifyAccount(ctx context.Context, cfg *config.Config, acc *store.Account) (status string, err error) {
	token := SSOToken(acc)
	if token == "" {
		return "", ErrMissingSSOToken
	}
	result := ProbeWebAccount(ctx, New(cfg), token, WebRefreshOptions{
		IdentityTimeout: 20 * time.Second, QuotaTimeout: 30 * time.Second, RetryDelay: time.Second,
	})
	ApplyWebRefresh(acc, result)
	acc.GrokWebSyncAt = time.Now().UTC()
	acc.GrokWebSyncError = ""
	switch {
	case result.AuthRejected:
		cause := result.QuotaErr
		if cause == nil {
			cause = result.IdentityErr
		}
		err = fmt.Errorf("grok.com rejected the SSO cookie: %w", cause)
		acc.GrokWebSyncError = "grok.com 不接受该 SSO Cookie，需要重新导入"
		return "401", err
	case result.QuotaErr != nil:
		// The quota read is what publishes the Web routes, so an account whose
		// identity resolved but whose quota did not is not synced.
		err = fmt.Errorf("grok.com quota read failed: %w", result.QuotaErr)
		acc.GrokWebSyncError = redactSSOError(err, acc)
		return "", err
	}
	acc.AuthStatus = store.AccountAuthStatusActive
	return "", nil
}

// Maintain re-reads a Web account's identity and quota on its own cadence and
// republishes the Web routes when the observation changed what is servable.
func Maintain(ctx context.Context, cfg *config.Config, st *store.Store, acc *store.Account) {
	if acc == nil || st == nil || !IsWebAccount(acc) || !acc.Enabled || !store.AccountAuthActive(acc) {
		return
	}
	if !acc.GrokWebQuota.SyncedAt.IsZero() && time.Since(acc.GrokWebQuota.SyncedAt) < webMaintenanceInterval {
		return
	}
	// A failing probe (an egress challenge, an upstream outage) leaves the
	// snapshot unsynced; without a floor the scheduler would retry it on every
	// tick.
	if last, ok := maintenanceAttempts.Load(acc.ID); ok && time.Since(last.(time.Time)) < webMaintenanceRetry {
		return
	}
	maintenanceAttempts.Store(acc.ID, time.Now())
	before := observedRouteSet([]*store.Account{acc})
	status, err := VerifyAccount(ctx, cfg, acc)
	now := time.Now()
	switch {
	case status == "401":
		acc.StatusCode = "401"
		acc.AuthStatus = store.AccountAuthStatusReauthRequired
		acc.StatusMessage = "grok.com 已不接受该 SSO Cookie，需要重新导入"
		acc.LastAttempt = now
	case err != nil:
		slog.Warn("grok web account maintenance failed", "account_id", acc.ID, "error", redactSSOError(err, acc))
	default:
		if strings.TrimSpace(acc.StatusCode) == "401" {
			acc.StatusCode = ""
			acc.StatusMessage = ""
		}
		acc.VerifiedAt = now
	}
	NormalizeProvider(acc)
	if err := st.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("grok web account maintenance: update failed", "account_id", acc.ID, "error", err)
		return
	}
	if after := observedRouteSet([]*store.Account{acc}); !sameRouteSet(before, after) || status == "401" {
		if err := PublishRoutes(ctx, st); err != nil {
			slog.Warn("grok web route publication failed", "error", err)
		}
	}
}

// accountObservesRoutes reports whether an account's stored observation is
// current evidence that it can serve Web routes.
func accountObservesRoutes(acc *store.Account, now time.Time) bool {
	if !IsWebAccount(acc) || !acc.Enabled || !store.AccountAuthActive(acc) || SSOToken(acc) == "" {
		return false
	}
	synced := acc.GrokWebQuota.SyncedAt
	return !synced.IsZero() && now.Sub(synced) < webObservationTTL
}

// observedRouteSet is the set of Web routes the given accounts' quota reads
// prove servable. A route is observed when some active account returned the
// quota window of the route's mode; the heavy route also needs a Heavy plan.
func observedRouteSet(accounts []*store.Account) map[string]ModelSpec {
	now := time.Now()
	out := map[string]ModelSpec{}
	for _, acc := range accounts {
		if !accountObservesRoutes(acc, now) {
			continue
		}
		quota := acc.GrokWebQuota
		hasAuto := quota.Auto.HasLimit || quota.Auto.HasRemaining
		hasFast := quota.Fast.HasLimit || quota.Fast.HasRemaining
		heavy := grokAccountPool(acc) == "heavy"
		for _, spec := range SupportedModels {
			switch {
			case spec.Tier == grokTierHeavy && !heavy:
				continue
			case spec.ModeID == "fast" && !hasFast:
				continue
			case spec.ModeID != "fast" && !hasAuto:
				continue
			}
			out[spec.ID] = spec
		}
	}
	return out
}

func sameRouteSet(a, b map[string]ModelSpec) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if _, ok := b[id]; !ok {
			return false
		}
	}
	return true
}

// PublishRoutes reconciles the Web rows of the grok model list with the routes
// the active Web accounts' quota reads observe. The reconciliation is scoped to
// the Web plane: Build rows are never touched. With no observing account the
// Web plane publishes nothing, so its rows are pruned.
func PublishRoutes(ctx context.Context, st *store.Store) error {
	if st == nil {
		return errors.New("store not configured")
	}
	accounts, err := st.ListAccounts(ctx)
	if err != nil {
		return err
	}
	observed := observedRouteSet(accounts)
	rows := make([]*store.Model, 0, len(observed))
	for index, spec := range SupportedModels {
		if _, ok := observed[spec.ID]; !ok {
			continue
		}
		row := &store.Model{
			Channel:       "grok",
			ModelID:       spec.ID,
			Name:          util.FirstNonEmpty(spec.Name, spec.ID),
			Status:        store.ModelStatusAvailable,
			Verified:      true,
			SortOrder:     1000 + index,
			Provider:      ProviderWeb,
			UpstreamModel: spec.UpstreamModel,
		}
		store.ApplyGrokWebRouteDefaults(row)
		rows = append(rows, row)
	}
	result, err := st.ReconcileDiscoveredModels(ctx, "grok", rows, store.ModelReconcileOptions{Prune: true, ProviderScope: ProviderWeb})
	if err != nil {
		return err
	}
	if result != nil && (result.Added > 0 || result.Deleted > 0) {
		slog.Info("Published Grok Web routes", "added", result.AddedModelIDs, "deleted", result.DeletedModelIDs)
	}
	return nil
}
