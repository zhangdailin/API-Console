package store

import "strings"

// IsGrokWeb reports whether the account is a Grok Web (grok.com SSO) account.
func (a *Account) IsGrokWeb() bool {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.AccountType), "grok") || strings.EqualFold(strings.TrimSpace(a.CredentialType), "oauth") {
		return false
	}
	if a.IsGrokConsole() {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(a.GrokProvider), GrokProviderWeb) || strings.EqualFold(strings.TrimSpace(a.CredentialType), "sso")
}

// IsGrokConsole reports whether the account is a Grok Console (console.x.ai
// SSO) account. A Console account carries the same SSO cookie as a Web
// account, so the provider field, not the credential, tells them apart.
func (a *Account) IsGrokConsole() bool {
	return a != nil && strings.EqualFold(strings.TrimSpace(a.AccountType), "grok") &&
		strings.EqualFold(strings.TrimSpace(a.GrokProvider), GrokProviderConsole)
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
