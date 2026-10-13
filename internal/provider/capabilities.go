package provider

import "orchids-api/internal/store"

// Capabilities is what the shared inference pipeline needs to know about a
// channel's catalog. It exists so internal/handler can ask a channel whether a
// model is supported or free without importing three provider packages to do it.
type Capabilities interface {
	// SupportsModel reports whether an account's catalog advertises the model.
	// An empty catalog means "unknown", which is treated as supported: refusing
	// an unseen model would break a channel whose snapshot has not synced yet.
	SupportsModel(acc *store.Account, model string) bool
	// IsFreeModel reports whether the model is currently free for the account.
	IsFreeModel(acc *store.Account, model string) bool
	// ContextWindows returns the input and output token budgets the channel
	// projects for an account's catalog. Either map may be nil.
	ContextWindows(acc *store.Account) (input, output map[string]int)
	// HonorsModelCooldown reports whether the channel's selection consults the
	// per-model cooldown its own verdicts write.
	HonorsModelCooldown() bool
}

// CapabilitiesFor returns the capability set a channel registered, or nil when
// it registered none. Grok registers none on purpose: it has its own handler
// and never asks these questions through this seam.
func CapabilitiesFor(accountType string) Capabilities {
	spec, ok := Lookup(accountType)
	if !ok {
		return nil
	}
	return spec.Capabilities
}

// SupportsModel reports whether the channel advertises the model for the
// account. It is true when no capabilities are registered: an unknown channel
// must not be blocked by a lookup this seam cannot answer.
func SupportsModel(accountType string, acc *store.Account, model string) bool {
	caps := CapabilitiesFor(accountType)
	if caps == nil {
		return true
	}
	return caps.SupportsModel(acc, model)
}

// IsFreeModel reports whether the channel treats the model as free.
func IsFreeModel(accountType string, acc *store.Account, model string) bool {
	caps := CapabilitiesFor(accountType)
	if caps == nil {
		return false
	}
	return caps.IsFreeModel(acc, model)
}

// ContextWindows returns the channel's projected token budgets.
func ContextWindows(accountType string, acc *store.Account) (input, output map[string]int) {
	caps := CapabilitiesFor(accountType)
	if caps == nil {
		return nil, nil
	}
	return caps.ContextWindows(acc)
}

// HonorsModelCooldown reports whether the channel scopes cooldowns per model.
func HonorsModelCooldown(accountType string) bool {
	caps := CapabilitiesFor(accountType)
	if caps == nil {
		return false
	}
	return caps.HonorsModelCooldown()
}
