package grok

import (
	"strings"

	"orchids-api/internal/config"
	"orchids-api/internal/modelpolicy"
)

// UpstreamKind selects which Grok upstream protocol serves a model.
type UpstreamKind int

const (
	// UpstreamAuto derives the upstream from ModelSpec fields and config.
	UpstreamAuto UpstreamKind = iota
	// UpstreamCLI is cli-chat-proxy.grok.com/v1 + OAuth Bearer.
	UpstreamCLI
)

// ModelSpec defines one public model and how it maps to Grok upstream fields.
type ModelSpec struct {
	ID            string
	Name          string
	UpstreamModel string
	// Upstream explicitly routes the model; UpstreamAuto derives from fields.
	Upstream UpstreamKind
	// AliasReasoningEffort is populated only while resolving an effort-suffixed
	// compatibility alias and is copied into the request before normalization.
	AliasReasoningEffort string
}

// SupportedModels is the deliberately small public compatibility table. Build
// OAuth capability snapshots remain authoritative; this table only describes the
// routes this gateway serves itself.
var SupportedModels = []ModelSpec{
	{ID: "grok-composer-2.5-fast", Name: "Grok Composer 2.5 Fast", UpstreamModel: "grok-composer-2.5-fast", Upstream: UpstreamCLI},
	{ID: "grok-4.5", Name: "Grok 4.5", UpstreamModel: "grok-4.5", Upstream: UpstreamCLI},
	{ID: "grok-4.6", Name: "Grok 4.6", UpstreamModel: "grok-4.6", Upstream: UpstreamCLI},
}

var modelByID = func() map[string]ModelSpec {
	out := make(map[string]ModelSpec, len(SupportedModels))
	for _, m := range SupportedModels {
		out[strings.ToLower(strings.TrimSpace(m.ID))] = m
	}
	return out
}()

// providerCompatibilityAliases preserve Build-qualified and historical names.
var providerCompatibilityAliases = map[string]string{
	"grok-4.5-latest":  "grok-4.5",
	"grok-4.6-latest":  "grok-4.6",
	"grok-code-fast":   "grok-composer-2.5-fast",
	"grok-code-fast-1": "grok-composer-2.5-fast",
	"build/grok-4.5":   "grok-4.5",
	"build/grok-4.6":   "grok-4.6",
}

func IsDeprecatedModelID(modelID string) bool {
	return modelpolicy.IsDeprecatedGrokModelID(normalizeModelID(modelID))
}

func normalizeModelID(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

// stripProviderPublicPrefix removes one provider qualifier, reporting whether it
// removed anything.
func stripProviderPublicPrefix(id string) (string, bool) {
	return modelpolicy.StripProviderPublicPrefix(id)
}

// ParseReasoningModelAlias resolves a supported <model>-<effort> alias. The
// suffix is accepted only when the base model's provider contract advertises
// that exact effort, so names such as grok-4.5-xhigh remain model-not-found.
func ParseReasoningModelAlias(modelID string) (base, effort string, ok bool) {
	id := normalizeModelID(modelID)
	for _, candidate := range []string{"xhigh", "medium", "high", "low", "none"} {
		if !strings.HasSuffix(id, "-"+candidate) {
			continue
		}
		base = strings.TrimSuffix(id, "-"+candidate)
		canonical := base
		if alias, exists := providerCompatibilityAliases[base]; exists {
			canonical = alias
		}
		if modelpolicy.SupportsReasoningEffort(canonical, candidate) {
			return base, candidate, true
		}
	}
	return "", "", false
}

func ResolveModelAlias(modelID string) (ModelSpec, string, bool) {
	id := normalizeModelID(modelID)
	// Exact model IDs always win.
	if m, exists := modelByID[id]; exists {
		return m, "", true
	}
	// Build-qualified spellings are accepted in any casing.
	if stripped, changed := stripProviderPublicPrefix(id); changed {
		if m, exists := modelByID[stripped]; exists {
			return m, "", true
		}
		if canonical, exists := providerCompatibilityAliases[stripped]; exists {
			m, found := modelByID[canonical]
			return m, "", found
		}
		id = stripped
	}
	if canonical, exists := providerCompatibilityAliases[id]; exists {
		m, found := modelByID[canonical]
		return m, "", found
	}
	if base, effort, exists := ParseReasoningModelAlias(id); exists {
		if canonical, aliased := providerCompatibilityAliases[base]; aliased {
			base = canonical
		}
		m, found := modelByID[base]
		return m, effort, found
	}
	return ModelSpec{}, "", false
}

func ResolveModel(modelID string) (ModelSpec, bool) {
	m, _, ok := ResolveModelAlias(modelID)
	return m, ok
}

// modelRoutedToCLI reports whether a resolved model uses Build CLI. The
// configured model table and dynamically discovered Build models explicitly
// carry UpstreamCLI; legacy config model lists no longer affect routing.
func modelRoutedToCLI(spec ModelSpec, _ *config.Config) bool {
	return spec.Upstream == UpstreamCLI
}
