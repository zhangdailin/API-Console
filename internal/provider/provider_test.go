package provider_test

import (
	"testing"

	"orchids-api/internal/channel"
	"orchids-api/internal/provider"
	_ "orchids-api/internal/provider/all"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// TestEveryDefinedChannelIsRegistered ties the two halves of a channel
// together: internal/channel says it exists, and its package has to say how it
// behaves. A channel defined but never linked would otherwise be routable and
// silently unserved.
func TestEveryDefinedChannelIsRegistered(t *testing.T) {
	registered := provider.All()
	testutil.Equal(t, len(registered), len(channel.All()))
	for index, definition := range channel.All() {
		spec, ok := provider.Lookup(string(definition.ID))
		testutil.True(t, ok, "channel %q has no registered spec")
		// All() follows definition order, so a caller that iterates it is
		// deterministic.
		testutil.Equal(t, registered[index].ID, definition.ID)

		// A generic channel is served by the shared pipeline, which needs a
		// client and catalog answers. A channel with its own handler must not
		// offer them, or the shared pipeline could be pointed at it.
		testutil.Equal(t, spec.NewClient != nil, definition.Generic)
		testutil.Equal(t, spec.Capabilities != nil, definition.Generic)

		testutil.True(t, spec.Catalog != nil && spec.Catalog.Observe != nil && spec.Catalog.LastKnown != nil,
			"channel %q must be able to read its upstream catalog")
		testutil.True(t, spec.Catalog.Source != "", "channel %q must name its catalog source")
		testutil.True(t, spec.Maintain != nil, "channel %q must maintain its accounts")
	}
}

func TestLookupNormalizesTheAccountType(t *testing.T) {
	for _, value := range []string{"qoder", "Qoder", " QODER "} {
		spec, ok := provider.Lookup(value)
		testutil.True(t, ok, "Lookup(%q) found nothing")
		testutil.Equal(t, spec.ID, channel.Qoder)
	}
	_, ok := provider.Lookup("unknown")
	testutil.False(t, ok, "an unknown account type must not resolve to a channel")
}

// TestRegisterRefusesWiringMistakes covers the two ways a registration can be
// wrong. Both would otherwise surface as a request served by the wrong
// implementation, so both must stop the process instead.
func TestRegisterRefusesWiringMistakes(t *testing.T) {
	mustPanic := func(name string, register func()) {
		t.Helper()
		defer func() {
			testutil.True(t, recover() != nil, name+" must panic")
		}()
		register()
	}
	mustPanic("registering an undefined channel", func() { provider.Register(provider.Spec{ID: "undefined"}) })
	mustPanic("registering a channel twice", func() { provider.Register(provider.Spec{ID: channel.Qoder}) })

	// The refused registrations must not have replaced the real one.
	spec, ok := provider.Lookup("qoder")
	testutil.True(t, ok && spec.NewClient != nil, "a refused registration replaced the registered channel")
}

func TestCapabilityHelpersAnswerSafelyForAnUnknownChannel(t *testing.T) {
	testutil.True(t, provider.SupportsModel("unknown", nil, "m"), "an unknown channel must not be blocked by a lookup this seam cannot answer")
	testutil.False(t, provider.IsFreeModel("unknown", nil, "m"), "an unknown channel has no free models")
	testutil.False(t, provider.HonorsModelCooldown("unknown"), "an unknown channel scopes no cooldowns")
	input, output := provider.ContextWindows("unknown", nil)
	testutil.True(t, input == nil && output == nil, "an unknown channel declares no context windows")
	// Grok has its own handler and registers no capabilities.
	testutil.True(t, provider.CapabilitiesFor("grok") == nil, "Grok must not answer through the shared-pipeline seam")
}

// TestEveryChannelServesTheManagementAPI keeps the admin surface whole. The
// management API asks a channel instead of knowing its fields, so a channel
// that leaves one of these out is an account that cannot be edited, verified,
// shown or deduplicated — and the handler would not notice.
func TestEveryChannelServesTheManagementAPI(t *testing.T) {
	for _, spec := range provider.All() {
		admin := spec.Admin
		testutil.True(t, admin != nil, "channel %q has no admin facet")
		hooks := map[string]bool{
			"CredentialKey": admin.CredentialKey != nil,
			"IdentityKey":   admin.IdentityKey != nil,
			"Fingerprint":   admin.Fingerprint != nil,
			"PrepareCreate": admin.PrepareCreate != nil,
			"PrepareUpdate": admin.PrepareUpdate != nil,
			"PrepareImport": admin.PrepareImport != nil,
			"Project":       admin.Project != nil,
			"ProjectQuota":  admin.ProjectQuota != nil,
			"Verify":        admin.Verify != nil,
		}
		for name, present := range hooks {
			testutil.Falsef(t, !present, "channel %q leaves Admin.%s unset", spec.ID, name)
		}
		testutil.True(t, admin.ErrMissingCredential != nil, "channel %q must name its missing-credential error")

		login := admin.Login
		testutil.True(t, login != nil, "channel %q has no login flow")
		testutil.Falsef(t, login.Start == nil || login.Exchange == nil || login.Build == nil || login.ErrPending == nil,
			"channel %q has an incomplete login flow", spec.ID)

		// The keys and the fingerprint are computed for rows the store returns,
		// including an empty one, and must not invent an identity for it.
		empty := &store.Account{AccountType: string(spec.ID)}
		testutil.Equal(t, admin.CredentialKey(empty), "")
		testutil.Equal(t, admin.IdentityKey(empty), "")
	}
}

// TestProjectNeverLeavesADurableCredential is the port-level half of the
// "credentials never reach clients" rule. The response marshaler deletes the
// credential keys again, but a channel must not depend on that: its own
// projection has to clear the token that can renew the account.
func TestProjectNeverLeavesADurableCredential(t *testing.T) {
	durable := map[channel.ID]func(*store.Account) []string{
		channel.WorkBuddy: func(acc *store.Account) []string { return []string{acc.WorkBuddyRefreshToken} },
		channel.Qoder: func(acc *store.Account) []string {
			return []string{acc.QoderRefreshToken, acc.QoderRuntimeInfo, acc.QoderRuntimeKey}
		},
		channel.Cline: func(acc *store.Account) []string { return []string{acc.ClineRefreshToken} },
		channel.Grok:  func(acc *store.Account) []string { return []string{acc.OAuthRefreshToken} },
	}
	for _, spec := range provider.All() {
		read, listed := durable[spec.ID]
		testutil.True(t, listed, "channel %q is not covered by the projection leak test; add its durable credential fields")

		stored := &store.Account{
			AccountType: string(spec.ID), CredentialType: "oauth",
			RefreshToken:          "generic-refresh",
			OAuthRefreshToken:     "oauth-refresh",
			WorkBuddyRefreshToken: "workbuddy-refresh",
			QoderRefreshToken:     "qoder-refresh", QoderRuntimeInfo: "qoder-info", QoderRuntimeKey: "qoder-key",
			ClineRefreshToken: "cline-refresh",
		}
		out := *stored
		spec.Admin.Project(&out)

		testutil.Equal(t, out.RefreshToken, "")
		for _, value := range read(&out) {
			testutil.Equal(t, value, "")
		}
		// The projection is given a copy; the stored record keeps what it holds.
		testutil.Equal(t, stored.RefreshToken, "generic-refresh")
	}
}
