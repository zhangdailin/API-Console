package store

import "strings"

// GrokPlane identifies the active provider for a stored Grok account.
// Infer the plane of unlabeled legacy accounts from their credential.
func (a *Account) GrokPlane() string {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.AccountType), "grok") || !SupportsGrokProvider(a.GrokProvider) {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(a.CredentialType), "oauth") {
		return GrokProviderBuild
	}
	if provider := strings.ToLower(strings.TrimSpace(a.GrokProvider)); provider != "" {
		return provider
	}
	if strings.EqualFold(strings.TrimSpace(a.CredentialType), "sso") || strings.TrimSpace(a.ClientCookie) != "" {
		return GrokProviderWeb
	}
	return GrokProviderBuild
}

// IsGrokWeb reports whether the account belongs to the Grok Web plane.
func (a *Account) IsGrokWeb() bool { return a.GrokPlane() == GrokProviderWeb }

// SupportsGrokProvider accepts the active provider names and unlabeled legacy
// Grok records. Explicit unsupported providers must never be inferred from credentials.
func SupportsGrokProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", GrokProviderWeb, GrokProviderBuild:
		return true
	default:
		return false
	}
}

// Secrets returns every credential-bearing value the account holds.
//
// It lives beside the type because the type is what grows new credential fields,
// and it is the one list every redactor must use. There were three hand-maintained
// copies: the account response redactor, the attempt-diagnostics scrubber and this
// one. They had already drifted — diagnostics redacted seven of the fourteen values
// — and each copy is a place where a newly added credential field is silently
// published.
//
// Callers that redact free text should replace each returned value, quoted or not:
// upstream errors echo credentials back in both forms.
func (a *Account) Secrets() []string {
	if a == nil {
		return nil
	}
	return []string{
		a.Token,
		a.ClientCookie,
		a.RefreshToken,
		a.OAuthAccessToken,
		a.OAuthRefreshToken,
		a.WorkBuddyAccessToken,
		a.WorkBuddyRefreshToken,
		a.ClineAccessToken,
		a.ClineRefreshToken,
		a.WarpAccessToken,
		a.WarpRefreshToken,
		a.DoubaoDeviceID,
		a.DoubaoWebID,
		a.DoubaoTeaUUID,
	}
}
