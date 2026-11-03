package web

import (
	"strings"

	"orchids-api/internal/store"
)

const (
	ProviderBuild = "build"
	ProviderWeb   = store.GrokProviderWeb
)

// ProviderForAccount reports the Grok plane an account belongs to. An OAuth
// credential is always Build; an explicit "web" provider or an SSO cookie
// credential is Web.
func ProviderForAccount(acc *store.Account) string {
	if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") {
		return ProviderBuild
	}
	switch strings.ToLower(strings.TrimSpace(acc.GrokProvider)) {
	case ProviderWeb:
		return ProviderWeb
	case ProviderBuild:
		return ProviderBuild
	}
	if strings.EqualFold(strings.TrimSpace(acc.CredentialType), "sso") || strings.TrimSpace(acc.ClientCookie) != "" {
		return ProviderWeb
	}
	return ProviderBuild
}

// IsWebAccount reports whether an account is a Grok Web SSO account.
func IsWebAccount(acc *store.Account) bool {
	return ProviderForAccount(acc) == ProviderWeb
}

// NormalizeProvider pins the provider field of a Web account.
func NormalizeProvider(acc *store.Account) bool {
	if !IsWebAccount(acc) {
		return false
	}
	changed := false
	if acc.GrokProvider != ProviderWeb {
		acc.GrokProvider = ProviderWeb
		changed = true
	}
	if acc.CredentialType != "sso" {
		acc.CredentialType = "sso"
		changed = true
	}
	return changed
}

// IsGrokWebSSOSource identifies an administrator-manageable Web SSO account.
func IsGrokWebSSOSource(acc *store.Account) bool {
	return IsWebAccount(acc)
}

// CollectWebSSOSourcesByToken returns Web SSO accounts grouped by normalized
// credential. For accidental duplicates the lowest ID is selected so callers
// never depend on backing-store iteration order.
func CollectWebSSOSourcesByToken(accounts []*store.Account, enabledOnly bool) map[string]*store.Account {
	result := make(map[string]*store.Account, len(accounts))
	for _, acc := range accounts {
		if !IsGrokWebSSOSource(acc) || (enabledOnly && !acc.Enabled) {
			continue
		}
		token := NormalizeSSOToken(grokSSOTokenRaw(acc))
		if token == "" {
			continue
		}
		if current := result[token]; current == nil || acc.ID < current.ID {
			result[token] = acc
		}
	}
	return result
}
