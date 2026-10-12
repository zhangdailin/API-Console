package api

import (
	"orchids-api/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin Cline-specific console contracts in assets a Go build does
// not compile. Shared provider-registry contracts live in the frontend suite;
// scripts/check-provider-registry.sh verifies the generated backend manifest.

// readConsoleScript loads one file out of web/static/js.
func readConsoleScript(name string) (string, error) {
	path := filepath.Join("..", "..", "web", "static", "js", filepath.Base(name))
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// TestAccountsJSPinsClineOfficialLoginLifecycle covers Cline-specific cleanup and
// official-only account creation, not the shared registry contract.
func TestAccountsJSPinsClineOfficialLoginLifecycle(t *testing.T) {
	source, err := readConsoleScript("accounts.js")
	testutil.NoError(t, err, "read accounts.js: %v")
	testutil.CheckContain(t, source, `OrchidsProviderRegistry?.get(key)`)
	// The login lifecycle must be stopped with the others: a transaction left
	// polling after the modal closes keeps exchanging a live device code.
	testutil.CheckContain(t, source, `function stopClineLogin()`)
	if calls := strings.Count(source, "stopClineLogin();"); calls < 2 {
		t.Errorf("stopClineLogin is called %d times, want at least 2 (open + close)", calls)
	}
	// All supported channels use official login for creation; only existing
	// accounts may submit their settings through this form.
	testutil.CheckContainAll(t, source, `if (!id) {`, `官方网页登录`)
}

// TestCommonJSPinsTheCredentialVerdictAndQuotaGuard pins the current account read
// contract: OAuth secrets stay server-side, while has_credential tells the UI
// whether the account can be used, and the same verdict backs the quota-only
// status guard. Cline's unmetered projection is shared by these surfaces too.
func TestCommonJSPinsTheCredentialVerdictAndQuotaGuard(t *testing.T) {
	source, err := readConsoleScript("common.js")
	testutil.NoError(t, err, "read common.js: %v")
	for _, want := range []string{
		`acc?.has_credential === true`,
		`isQuotaOnlyStatus`,
		`unmetered: true`,
	} {
		testutil.CheckContain(t, source, want)
	}
}

// TestModelsJSLabelsClineCatalogSource protects the Cline-specific refresh source
// label; provider tab membership is covered by the frontend registry suite.
func TestModelsJSLabelsClineCatalogSource(t *testing.T) {
	source, err := readConsoleScript("models.js")
	testutil.NoError(t, err, "read models.js: %v")
	testutil.CheckContain(t, source, "cline_recommended_models")
}

// TestAccountsJSRendersTheClineRowCells protects the plan, quota, and
// credential-verdict rendering used on the Cline account page.
func TestAccountsJSRendersTheClineRowCells(t *testing.T) {
	source, err := readConsoleScript("accounts.js")
	testutil.NoError(t, err, "read accounts.js: %v")
	// 配额: an unmetered channel is now dropped from the Cline page rather than
	// rendered as a permanent "未计量". common.js owns the shared quota verdict;
	// accounts.js renders it on surfaces that still show the cell.
	testutil.CheckContain(t, source, "未计量")
	testutil.CheckContain(t, source, "clinePageOnly")
	// 等级: the tier now comes from the upstream plan endpoint. What is pinned
	// is that the badge reads that field at all — a tier inferred from the
	// catalog cannot tell a free account from a subscriber's, which is exactly
	// the mistake the old "免费目录" label made.
	testutil.CheckContain(t, source, `cline_plan`)
	// 状态: the credential verdict must come from has_credential, not from the
	// session columns this channel never writes.
	testutil.CheckContainAll(t, source, `cline: '缺少 Cline WorkOS 凭据`, `hasSidebarAccountCredential(acc)`)
}
