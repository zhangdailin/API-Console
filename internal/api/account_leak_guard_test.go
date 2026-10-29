package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// accountSecretFields is every field on a stored account that carries a
// credential, a session token or material derived from one.
//
// The list is the guard's whole point: a new credential field that nobody adds
// here is one the account API could start returning without a test noticing, so
// TestAccountSecretFieldsAreComplete fails when the struct grows one.
var accountSecretFields = map[string]func(*store.Account) string{
	"token":                   func(a *store.Account) string { return a.Token },
	"client_cookie":           func(a *store.Account) string { return a.ClientCookie },
	"refresh_token":           func(a *store.Account) string { return a.RefreshToken },
	"oauth_access_token":      func(a *store.Account) string { return a.OAuthAccessToken },
	"oauth_refresh_token":     func(a *store.Account) string { return a.OAuthRefreshToken },
	"workbuddy_access_token":  func(a *store.Account) string { return a.WorkBuddyAccessToken },
	"workbuddy_refresh_token": func(a *store.Account) string { return a.WorkBuddyRefreshToken },
	"qoder_access_token":      func(a *store.Account) string { return a.QoderAccessToken },
	"qoder_refresh_token":     func(a *store.Account) string { return a.QoderRefreshToken },
	"qoder_runtime_info":      func(a *store.Account) string { return a.QoderRuntimeInfo },
	"qoder_runtime_key":       func(a *store.Account) string { return a.QoderRuntimeKey },
	"cline_access_token":      func(a *store.Account) string { return a.ClineAccessToken },
	"cline_refresh_token":     func(a *store.Account) string { return a.ClineRefreshToken },
}

// TestAccountSecretFieldsAreComplete makes every new Account field require an
// explicit security review. Name heuristics alone miss credentials called things
// like runtime_info, so even non-secret fields are explicitly classified here.
func TestAccountSecretFieldsAreComplete(t *testing.T) {
	nonSecret := map[string]bool{}
	for name := range strings.FieldsSeq(`
		ID Name AccountType UserID AgentMode Email Weight MaxConcurrent Enabled
		Subscription UsageCurrent UsageTotal UsageLimit TokensToday TokensDate
		StatusCode AuthStatus RateLimitFailures QualityFailures QualityRevision QualityCooldownUntil QualityReason
		StatusMessage LastAttempt VerifiedAt ClearVerifiedAt QuotaResetAt RequestCount
		LastUsedAt CreatedAt UpdatedAt CredentialType OAuthExpiresAt TeamID
		GrokProvider GrokModels GrokModelCatalog GrokModelsSyncedAt GrokBilling
		GrokRateLimits GrokFreeQuota ModelCooldowns ModelCooldownReasons
		WorkBuddyExpiresAt WorkBuddyUID ReplaceWorkBuddyCredentials WorkBuddyModelIDs
		WorkBuddyModelsSyncedAt WorkBuddyQuota WorkBuddyRestrictedUntil QoderExpiresAt ReplaceQoderCredentials
		QoderMachineID QoderUserID QoderUserName QoderOrganizationID
		QoderOrganizationTags QoderDataPolicy QoderRuntimeIdentity QoderModelIDs QoderModelsSyncedAt QoderQuota
		ClineExpiresAt ClineEmail ClinePlan ReplaceClineCredentials ClineModelIDs
		ClineModelsSyncedAt
	`) {
		nonSecret[name] = true
	}
	accountType := reflect.TypeOf(store.Account{})
	seenSecrets := map[string]bool{}
	seenNonSecrets := map[string]bool{}
	for i := 0; i < accountType.NumField(); i++ {
		field := accountType.Field(i)
		key, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		read, secret := accountSecretFields[key]
		testutil.CheckFalsef(t, secret && nonSecret[field.Name], "%s is classified as both secret and non-secret", field.Name)
		if !secret {
			testutil.CheckFalsef(t, !nonSecret[field.Name], "new account field %s (%q) needs explicit secret/non-secret classification", field.Name, key)
			seenNonSecrets[field.Name] = true
			continue
		}
		seenSecrets[key] = true
		if field.Type.Kind() != reflect.String {
			t.Errorf("secret field %s needs a guard for its new type %s", field.Name, field.Type)
			continue
		}
		set, ok := accountSecretSetter[key]
		if !ok {
			t.Errorf("secret field %q has no setter", key)
			continue
		}
		acc := &store.Account{}
		value := marker + key
		set(acc, value)
		testutil.CheckEqual(t, read(acc), value)
		testutil.CheckEqual(t, reflect.ValueOf(acc).Elem().Field(i).String(), value)
	}
	for key := range accountSecretFields {
		testutil.CheckFalsef(t, !seenSecrets[key], "secret guard %q has no corresponding Account field", key)
	}
	for key := range accountSecretSetter {
		_, ok := accountSecretFields[key]
		testutil.CheckFalsef(t, !ok, "secret setter %q has no corresponding getter", key)
	}
	for name := range nonSecret {
		testutil.CheckFalsef(t, !seenNonSecrets[name], "non-secret classification %q has no corresponding Account field", name)
	}
}

// accountChannels are the channels the account API serves. Every one of them goes
// through the same projection, so the guard has to cover each.
var accountChannels = []string{"grok", "workbuddy", "qoder", "cline"}

// marker prefixes every planted secret so one substring search can find all of
// them in the rendered JSON, whatever the field name became on the wire.
const marker = "PLANTED-SECRET-"

func TestAccountQualityReasonIsOperationalAndRedacted(t *testing.T) {
	acc := &store.Account{AccountType: "grok", Enabled: true, QualityFailures: 1,
		QualityReason: "no_reasoning", OAuthAccessToken: marker + "quality-reason"}
	raw, err := json.Marshal(normalizeAccountOutput(acc))
	testutil.NoError(t, err, "marshal account: %v")
	testutil.MustContain(t, string(raw), `"quality_reason":"no_reasoning"`)
	acc.QualityReason = "upstream echoed " + acc.OAuthAccessToken
	raw, err = json.Marshal(normalizeAccountOutput(acc))
	testutil.NoError(t, err, "marshal redacted account: %v")
	testutil.MustNotContain(t, string(raw), marker)
}

// accountWithSecrets builds an account of one channel with a distinct value in
// every credential field.
func accountWithSecrets(channel string) *store.Account {
	acc := &store.Account{ID: 42, Name: "guard", AccountType: channel, Enabled: true}
	for field, read := range accountSecretFields {
		switch read(acc) {
		case "":
			set := accountSecretSetter[field]
			set(acc, marker+field)
		}
	}
	// The status message carries upstream text verbatim, and upstream text is
	// exactly where a credential ends up when a request is echoed back. Plant
	// every value there too: both redaction layers have to strip them.
	acc.StatusMessage = "upstream rejected the request"
	for _, read := range accountSecretFields {
		if value := read(acc); value != "" {
			acc.StatusMessage += " " + value
		}
	}
	return acc
}

// accountSecretSetter mirrors accountSecretFields for the write direction.
var accountSecretSetter = map[string]func(*store.Account, string){
	"token":                  func(a *store.Account, v string) { a.Token = v },
	"client_cookie":          func(a *store.Account, v string) { a.ClientCookie = v },
	"refresh_token":          func(a *store.Account, v string) { a.RefreshToken = v },
	"oauth_access_token":     func(a *store.Account, v string) { a.OAuthAccessToken = v },
	"oauth_refresh_token":    func(a *store.Account, v string) { a.OAuthRefreshToken = v },
	"workbuddy_access_token": func(a *store.Account, v string) { a.WorkBuddyAccessToken = v },
	"workbuddy_refresh_token": func(a *store.Account, v string) {
		a.WorkBuddyRefreshToken = v
	},
	"qoder_access_token":  func(a *store.Account, v string) { a.QoderAccessToken = v },
	"qoder_refresh_token": func(a *store.Account, v string) { a.QoderRefreshToken = v },
	"qoder_runtime_info":  func(a *store.Account, v string) { a.QoderRuntimeInfo = v },
	"qoder_runtime_key":   func(a *store.Account, v string) { a.QoderRuntimeKey = v },
	"cline_access_token":  func(a *store.Account, v string) { a.ClineAccessToken = v },
	"cline_refresh_token": func(a *store.Account, v string) { a.ClineRefreshToken = v },
}

// TestAccountResponsesNeverCarryCredentials is the guard the account projection's
// refactor depends on.
//
// normalizeAccountOutputWithUsage is table-driven per channel, and its redaction
// is the last thing standing between an upstream error body and the management
// API. The test plants a distinct secret in every credential field and asserts
// none of them survives rendering, for every channel — including the fields of
// channels an account is not using, because a legacy row can hold another
// channel's value in a shared slot.
func TestAccountResponsesNeverCarryCredentials(t *testing.T) {
	for _, channel := range accountChannels {
		t.Run(channel, func(t *testing.T) {
			acc := accountWithSecrets(channel)
			raw, err := json.Marshal(normalizeAccountOutput(acc))
			testutil.NoError(t, err, "marshal account output: %v")
			leaked := findPlantedSecrets(raw)
			testutil.Falsef(t, len(leaked) > 0, "%s account response leaked %v\n%s", channel, leaked, raw)
			// Presence, not the value, is how the table proves a credential exists.
			var row map[string]any
			testutil.NoError(t, json.Unmarshal(raw, &row), "decode rendered account: %v")
			testutil.Equal(t, row["has_credential"], true)
		})
	}
}

// TestAccountResponsesHideCredentialKeys proves the credential keys are absent
// rather than merely empty, so a client cannot distinguish "no credential" from
// "credential withheld" by key presence.
func TestAccountResponsesHideCredentialKeys(t *testing.T) {
	acc := accountWithSecrets("qoder")
	raw, err := json.Marshal(normalizeAccountOutput(acc))
	testutil.NoError(t, err, "marshal account output: %v")
	var row map[string]any
	testutil.NoError(t, json.Unmarshal(raw, &row), "decode rendered account: %v")
	for field := range accountSecretFields {
		_, exists := row[field]
		testutil.CheckFalsef(t, exists, "credential field %q was returned", field)
	}
	for _, derived := range []string{"session_fingerprint"} {
		_, exists := row[derived]
		testutil.CheckFalsef(t, exists, "derived field %q was returned", derived)
	}
}

// findPlantedSecrets returns the planted field names still visible in raw.
func findPlantedSecrets(raw []byte) []string {
	text := string(raw)
	found := []string{}
	for field := range accountSecretFields {
		if strings.Contains(text, marker+field) {
			found = append(found, field)
		}
	}
	return found
}
