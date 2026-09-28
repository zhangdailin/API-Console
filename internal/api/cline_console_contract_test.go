package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The console renders the account channel strip from a hard-coded list. A channel
// missing from that list has no tab, so its login group in the account modal can
// never be selected — the channel is unreachable from the console no matter how
// correct the server side is. These tests pin the console's own contract, which
// lives in files a Go build never compiles.

// readConsoleScript loads one file out of web/static/js.
func readConsoleScript(name string) (string, error) {
	path := filepath.Join("..", "..", "web", "static", "js", filepath.Base(name))
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// TestAccountsJSListClineInTheChannelStrip asserts both console pages consume
// the shared registry and that the registry contains the Cline key and label.
func TestAccountsJSListClineInTheChannelStrip(t *testing.T) {
	source, err := readConsoleScript("accounts.js")
	if err != nil {
		t.Fatalf("read accounts.js: %v", err)
	}
	registry, err := readConsoleScript("provider-registry.js")
	if err != nil {
		t.Fatalf("read provider-registry.js: %v", err)
	}
	if !strings.Contains(source, "OrchidsProviderRegistry?.keys") || !strings.Contains(source, "OrchidsProviderRegistry?.providers") {
		t.Error("accounts.js does not consume the shared provider registry")
	}
	if !strings.Contains(registry, `Code generated from internal/channel definitions`) {
		t.Error("the frontend registry is not generated from the backend provider registry")
	}
	if !strings.Contains(source, `OrchidsProviderRegistry?.get(key)`) {
		t.Error("accounts.js does not resolve the cline account type")
	}
	// The login lifecycle must be stopped with the others: a transaction left
	// polling after the modal closes keeps exchanging a live device code.
	if !strings.Contains(source, `function stopClineLogin()`) {
		t.Error("accounts.js never stops the Cline login")
	}
	if calls := strings.Count(source, "stopClineLogin();"); calls < 2 {
		t.Errorf("stopClineLogin is called %d times, want at least 2 (open + close)", calls)
	}
	// All supported channels use official login for creation; only existing
	// accounts may submit their settings through this form.
	if !strings.Contains(source, `if (!id) {`) || !strings.Contains(source, `官方网页登录`) {
		t.Error("accounts.js lets a new account be submitted without official login")
	}
}

// TestCommonJSUsesTheServerCredentialVerdict pins the current account read
// contract: OAuth secrets stay server-side, while has_credential tells the UI
// whether the account can be used.
func TestCommonJSUsesTheServerCredentialVerdict(t *testing.T) {
	source, err := readConsoleScript("common.js")
	if err != nil {
		t.Fatalf("read common.js: %v", err)
	}
	if !strings.Contains(source, `acc?.has_credential === true`) {
		t.Error("common.js does not use the server's explicit credential verdict")
	}
}

// TestModelsJSListClineInTheChannelStrip pins the models page: without the
// channel there, a refreshed Cline catalog has no tab to appear under.
func TestModelsJSListClineInTheChannelStrip(t *testing.T) {
	source, err := readConsoleScript("models.js")
	if err != nil {
		t.Fatalf("read models.js: %v", err)
	}
	if !strings.Contains(source, "OrchidsProviderRegistry?.channels") {
		t.Error("models.js does not consume the shared provider registry")
	}
	if !strings.Contains(source, "cline_recommended_models") {
		t.Error("models.js does not label the Cline catalog source")
	}
}

// TestAccountsJSRendersTheClineRowCells protects the plan, quota, and
// credential-verdict rendering used on the Cline account page.
func TestAccountsJSRendersTheClineRowCells(t *testing.T) {
	source, err := readConsoleScript("accounts.js")
	if err != nil {
		t.Fatalf("read accounts.js: %v", err)
	}
	// 配额: an unmetered channel is now dropped from the Cline page rather than
	// rendered as a permanent "未计量". The verdict itself stays in the source
	// for every other surface that still renders the cell.
	if !strings.Contains(source, "unmetered: true") {
		t.Error("getQuotaStats has no unmetered verdict for Cline")
	}
	if !strings.Contains(source, "未计量") {
		t.Error("buildQuotaMarkup does not name the unmetered verdict")
	}
	if !strings.Contains(source, "clinePageOnly") {
		t.Error("the 配额 column is not hidden on the Cline page")
	}
	// 等级: the tier now comes from the upstream plan endpoint. What is pinned
	// is that the badge reads that field at all — a tier inferred from the
	// catalog cannot tell a free account from a subscriber's, which is exactly
	// the mistake the old "免费目录" label made.
	if !strings.Contains(source, `cline_plan`) {
		t.Error("subscriptionBadge does not read the Cline plan the server observed")
	}
	// 状态: the credential verdict must come from has_credential, not from the
	// session columns this channel never writes.
	if !strings.Contains(source, `cline: '缺少 Cline WorkOS 凭据`) || !strings.Contains(source, `hasSidebarAccountCredential(acc)`) {
		t.Error("evaluateAccountStatus does not use the server credential verdict for Cline")
	}
}

// TestCommonJSCountsTheClineCredentialPresence pins the verdict behind the whole
// row: without has_credential the status cell cannot know if Cline is authorized.
func TestCommonJSCountsTheClineCredentialPresence(t *testing.T) {
	source, err := readConsoleScript("common.js")
	if err != nil {
		t.Fatalf("read common.js: %v", err)
	}
	if !strings.Contains(source, `isQuotaOnlyStatus`) {
		t.Error("common.js has no quota-only guard")
	}
}
