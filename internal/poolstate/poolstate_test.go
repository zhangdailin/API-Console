package poolstate

import (
	"strings"
	"testing"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// The two original implementations this package replaces, kept verbatim so the
// merge can be checked against them rather than against a re-reading of the
// intent. Both are used only below.
func legacyAlertPoolCounts(accounts []*store.Account, channel string, now time.Time) (enabled, available, needingLogin, modelCooldowns int) {
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(acc.AccountType), channel) || !acc.Enabled {
			continue
		}
		enabled++
		if !accountpolicy.AccountHeld(acc, now) {
			available++
		}
		if accountpolicy.NeedsReverify(acc, now) {
			needingLogin++
		}
		for model, until := range acc.ModelCooldowns {
			if strings.TrimSpace(model) != "" && until.After(now) {
				modelCooldowns++
			}
		}
	}
	return enabled, available, needingLogin, modelCooldowns
}

func legacyConsolePoolCounts(accounts []*store.Account, channel string, now time.Time) (enabled, available, needingLogin, modelCooldowns int) {
	for _, acc := range accounts {
		if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), channel) {
			continue
		}
		if !acc.Enabled {
			continue
		}
		enabled++
		if strings.TrimSpace(acc.StatusCode) == "401" {
			needingLogin++
		}
		for model, until := range acc.ModelCooldowns {
			if strings.TrimSpace(model) != "" && until.After(now) {
				modelCooldowns++
			}
		}
		if accountpolicy.AccountHeld(acc, now) {
			continue
		}
		available++
	}
	return enabled, available, needingLogin, modelCooldowns
}

// Every account shape that changes one of the four counters must produce the
// same numbers through the merged function as through the original it replaces.
func TestCountsMatchesBothOriginalImplementations(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	shapes := []*store.Account{
		nil,
		{AccountType: "grok", Enabled: true},
		{AccountType: "grok", Enabled: false},
		{AccountType: "grok", Enabled: true, StatusCode: "401"},
		{AccountType: "grok", Enabled: true, StatusCode: "401", VerifiedAt: past},
		{AccountType: "grok", Enabled: true, StatusCode: "401", VerifiedAt: future},
		{AccountType: "grok", Enabled: true, StatusCode: "429"},
		{AccountType: "GROK", Enabled: true, StatusCode: "401"},
		{AccountType: " grok ", Enabled: true},
		{AccountType: "workbuddy", Enabled: true, StatusCode: "401"},
		{AccountType: "grok", Enabled: true, ModelCooldowns: map[string]time.Time{"m1": future}},
		{AccountType: "grok", Enabled: true, ModelCooldowns: map[string]time.Time{"m1": past}},
		{AccountType: "grok", Enabled: true, ModelCooldowns: map[string]time.Time{"": future}},
	}

	for _, channel := range []string{"grok", "workbuddy"} {
		for _, accounts := range [][]*store.Account{nil, shapes} {
			wantEnabled, wantAvailable, wantNeeding, wantCooldowns := legacyAlertPoolCounts(accounts, channel, now)
			wantConsoleEnabled, wantConsoleAvailable, wantConsoleNeeding, wantConsoleCooldowns := legacyConsolePoolCounts(accounts, channel, now)

			gotAlert := Counts(accounts, channel, now, NeedingLoginReverify)
			testutil.Equal(t, gotAlert.Enabled, wantEnabled)
			testutil.Equal(t, gotAlert.Available, wantAvailable)
			testutil.Equal(t, gotAlert.NeedingLogin, wantNeeding)
			testutil.Equal(t, gotAlert.ModelCooldowns, wantCooldowns)

			gotConsole := Counts(accounts, channel, now, NeedingLoginRefused)
			testutil.Equal(t, gotConsole.Enabled, wantConsoleEnabled)
			testutil.Equal(t, gotConsole.Available, wantConsoleAvailable)
			testutil.Equal(t, gotConsole.NeedingLogin, wantConsoleNeeding)
			testutil.Equal(t, gotConsole.ModelCooldowns, wantConsoleCooldowns)
		}
	}
}
