package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// The account export is a downloadable, shareable artifact, so it is a different
// surface from the account API: it marshals the stored record directly to stay
// re-importable, which means it does not pass through accountOutput.MarshalJSON
// and its key-stripping. These tests pin the guarantees that gap would otherwise
// leave unstated.

// exportedCredentialKeys exports one account of the given channel and returns the
// JSON keys whose value is one of the planted secrets.
func exportedCredentialKeys(t *testing.T, acc *store.Account) []string {
	t.Helper()
	s, _ := newTestStore(t, "export-guard-"+acc.AccountType+":")
	cfg := &config.Config{AdminPass: "x"}
	a := New(s, "admin", cfg.AdminPass, cfg)
	testutil.NoError(t, s.CreateAccount(t.Context(), acc), "create account: %v")
	rec := httptest.NewRecorder()
	a.HandleExport(rec, httptest.NewRequest(http.MethodGet, "/api/export", nil))
	testutil.Equal(t, rec.Code, http.StatusOK)
	var payload struct {
		Accounts []map[string]any `json:"accounts"`
	}
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload), "decode export: %v")
	testutil.Equal(t, len(payload.Accounts), 1)
	keys := []string{}
	for _, row := range payload.Accounts {
		for key, value := range row {
			if text, ok := value.(string); ok && strings.HasPrefix(text, marker) {
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// TestExportNeverCarriesAnotherChannelsCredential is the guard for the gap.
//
// A legacy row can hold a value in a slot its own channel never writes. The
// account API hides that by stripping credential keys from the response; the
// export has to hide it in data instead, because it marshals the record itself.
// The planted account deliberately carries a credential in every channel's slots,
// which is exactly the shape a row migrated from an older release can have.
func TestExportNeverCarriesAnotherChannelsCredential(t *testing.T) {
	// Which planted slots each channel may legitimately export. The generic slots
	// are included because every channel's resolver may fall back to them for a
	// credential document written before the channel had fields of its own.
	allowed := map[string]map[string]bool{
		"grok":      setOf("client_cookie", "refresh_token", "token", "oauth_access_token", "oauth_refresh_token"),
		"workbuddy": setOf("client_cookie", "refresh_token", "token", "workbuddy_access_token", "workbuddy_refresh_token"),
		"cline":     setOf("client_cookie", "refresh_token", "token", "cline_access_token", "cline_refresh_token"),
		"qoder": setOf("client_cookie", "refresh_token", "token", "qoder_access_token", "qoder_refresh_token",
			"qoder_runtime_info", "qoder_runtime_key"),
	}
	for _, channel := range accountChannels {
		t.Run(channel, func(t *testing.T) {
			acc := accountWithSecrets(channel)
			acc.ID, acc.Weight = 1, 1
			got := exportedCredentialKeys(t, acc)
			testutil.NotEqual(t, len(got), 0)
			for _, key := range got {
				testutil.CheckFalsef(t, !allowed[channel][key], "export of a %s account carries %q, which belongs to another channel", channel, key)
			}
		})
	}
}

// TestExportKeepsOAuthCredentialsForAnOAuthAccount pins the one place where the
// export is deliberately more revealing than the account API: an OAuth export that
// dropped its credential could not be re-imported, and there is no other way to
// recreate one.
func TestExportKeepsOAuthCredentialsForAnOAuthAccount(t *testing.T) {
	acc := &store.Account{
		ID: 1, Name: "oauth", AccountType: "grok", CredentialType: "oauth",
		OAuthAccessToken:  marker + "oauth_access_token",
		OAuthRefreshToken: marker + "oauth_refresh_token",
		Enabled:           true, Weight: 1,
	}
	got := exportedCredentialKeys(t, acc)
	for _, want := range []string{"oauth_access_token", "oauth_refresh_token"} {
		testutil.CheckTrue(t, containsString(got, want), "OAuth export dropped %q; it must stay re-importable (got %v)")
	}
	// The account API still hides them: the export is the only surface allowed to
	// carry them.
	raw, err := json.Marshal(normalizeAccountOutput(acc))
	testutil.NoError(t, err, "marshal account output: %v")
	testutil.MustNotContain(t, string(raw), marker)
}

func setOf(keys ...string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestExportCarriesTheDurableCredentialForReimport pins the other half of the
// contract: the export is a portability artifact, so a channel whose only way to
// renew is a rotated refresh token must export it.
//
// Without the refresh token a restored WorkBuddy or Qoder account works until its
// access token expires and then cannot recover — there is no other renewal path —
// which made the export unusable for exactly the channels the file exists to move.
func TestExportCarriesTheDurableCredentialForReimport(t *testing.T) {
	cases := map[string]struct {
		acc  *store.Account
		want []string
	}{
		"workbuddy": {
			acc: &store.Account{ID: 1, AccountType: "workbuddy", Enabled: true, Weight: 1,
				WorkBuddyAccessToken: marker + "access", WorkBuddyRefreshToken: marker + "refresh"},
			want: []string{"workbuddy_access_token", "workbuddy_refresh_token"},
		},
		"cline": {
			acc: &store.Account{ID: 1, AccountType: "cline", Enabled: true, Weight: 1,
				ClineAccessToken: marker + "access", ClineRefreshToken: marker + "refresh"},
			want: []string{"cline_access_token", "cline_refresh_token"},
		},
		"qoder": {
			acc: &store.Account{ID: 1, AccountType: "qoder", Enabled: true, Weight: 1,
				QoderAccessToken: marker + "access", QoderRefreshToken: marker + "refresh",
				QoderRuntimeInfo: marker + "info", QoderRuntimeKey: marker + "key"},
			want: []string{"qoder_access_token", "qoder_refresh_token", "qoder_runtime_info", "qoder_runtime_key"},
		},
	}
	for channel, tc := range cases {
		t.Run(channel, func(t *testing.T) {
			got := exportedCredentialKeys(t, tc.acc)
			for _, want := range tc.want {
				testutil.CheckTrue(t, containsString(got, want), "export dropped %q; the account could not be renewed after import (got %v)")
			}
		})
	}
}

// TestAccountCheckKeepsASpentAllowanceVerdict pins the console behaviour: a check
// must not clear a verdict the selector still enforces.
//
// The check verifies the credential, and the credential is genuinely fine — what
// parked the account was the upstream refusing an actual request for lack of
// allowance. Clearing the marker on a successful check made the account turn green
// and then fail again on the very next request, which reads as a flapping gateway
// rather than an exhausted one.
func TestAccountCheckKeepsASpentAllowanceVerdict(t *testing.T) {
	now := time.Now()
	parked := &store.Account{
		ID: 1, AccountType: "workbuddy", StatusCode: "402",
		StatusMessage: "Credits exhausted. Please visit the link below to purchase add-on packs",
		LastAttempt:   now.Add(-time.Minute),
		QuotaResetAt:  now.Add(24 * time.Hour),
		UsageLimit:    250, UsageCurrent: 0, // the fresh meter still reports it spent
	}
	applySuccessfulAccountRefreshStatus(parked, "")
	testutil.Equal(t, parked.StatusCode, "402")
	testutil.NotEqual(t, parked.StatusMessage, "")
	testutil.False(t, parked.VerifiedAt.IsZero(), "the check must still record that the credential was exercised")

	// Once the reset time has passed the selector would release the account, so the
	// check must be free to clear the marker too.
	reset := now.Add(-time.Minute)
	expired := &store.Account{ID: 2, AccountType: "workbuddy", StatusCode: "402", QuotaResetAt: reset,
		UsageLimit: 250, UsageCurrent: 0}
	applySuccessfulAccountRefreshStatus(expired, "")
	testutil.Equal(t, expired.StatusCode, "")

	// A meter that reports credits again releases the park immediately, so an
	// operator who tops up does not wait for the cycle boundary.
	toppedUp := &store.Account{ID: 4, AccountType: "workbuddy", StatusCode: "402",
		QuotaResetAt: now.Add(24 * time.Hour), UsageLimit: 250, UsageCurrent: 250}
	applySuccessfulAccountRefreshStatus(toppedUp, "")
	testutil.Equal(t, toppedUp.StatusCode, "")

	// A verdict with no reset time is a cooldown, not an allowance: the check still
	// clears it.
	cooldown := &store.Account{ID: 3, AccountType: "workbuddy", StatusCode: "402"}
	applySuccessfulAccountRefreshStatus(cooldown, "")
	testutil.Equal(t, cooldown.StatusCode, "")
}

// TestAccountCheckKeepsTheReasonForAnUnchangedVerdict pins that a bare status from
// a verifier does not erase the explanation an operator already has.
//
// Some verify paths report "402" with no message of their own. A manual check used
// to replace the upstream's "No usage left for request" with an empty string, so the
// account stayed parked and the table stopped saying why.
func TestAccountCheckKeepsTheReasonForAnUnchangedVerdict(t *testing.T) {
	reason := "upstream API error: status=402, body={\"code\":\"insufficient_funds\"}"

	// Same status: the specific reason survives.
	same := &store.Account{AccountType: "workbuddy", StatusCode: "402", StatusMessage: reason}
	applySuccessfulAccountRefreshStatus(same, "402")
	testutil.Equal(t, same.StatusMessage, reason)

	// Different status: an old reason described a different problem and must not be
	// carried onto the new one.
	different := &store.Account{AccountType: "workbuddy", StatusCode: "401", StatusMessage: "session expired"}
	applySuccessfulAccountRefreshStatus(different, "402")
	testutil.MustNotContain(t, different.StatusMessage, "session expired")

	// A verifier with something to say keeps its own wording.
	explicit := &store.Account{AccountType: "qoder", StatusCode: "402", StatusMessage: "old"}
	applySuccessfulAccountRefreshStatus(explicit, "402")
	testutil.Equal(t, explicit.StatusMessage, "old")
}
