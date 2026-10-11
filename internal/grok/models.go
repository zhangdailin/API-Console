package grok

import (
	"strings"

	"orchids-api/internal/config"
	"orchids-api/internal/modelcatalog"
)

// UpstreamKind selects which Grok upstream protocol serves a model.
type UpstreamKind int

const (
	// UpstreamAuto is the unspecified zero value; it does not select an upstream.
	UpstreamAuto UpstreamKind = iota
	// UpstreamCLI is cli-chat-proxy.grok.com/v1 + OAuth Bearer.
	UpstreamCLI
)

// ModelSpec defines one public model and how it maps to Grok upstream fields.
type ModelSpec struct {
	ID            string
	Name          string
	UpstreamModel string
	// Upstream explicitly routes the model; served models use UpstreamCLI.
	Upstream UpstreamKind
	// AliasReasoningEffort is populated only while resolving an effort-suffixed
	// model variant and is copied into the request before normalization.
	AliasReasoningEffort string
}

func normalizeModelID(modelID string) string { return strings.ToLower(strings.TrimSpace(modelID)) }

// ResolveModelAlias accepts only identifiers and effort menus observed upstream.
// Exact IDs win even when their last component resembles an effort alias.
func ResolveModelAlias(modelID string, catalogs ...[]modelcatalog.Profile) (ModelSpec, string, bool) {
	id := normalizeModelID(modelID)
	profiles := modelcatalog.Aggregate(catalogs...)
	makeSpec := func(p modelcatalog.Profile) ModelSpec {
		return ModelSpec{ID: normalizeModelID(p.ModelID), Name: p.ModelID, UpstreamModel: p.ModelID, Upstream: UpstreamCLI}
	}
	for _, profile := range profiles {
		if normalizeModelID(profile.ModelID) == id && id != "" {
			return makeSpec(profile), "", true
		}
	}
	for _, profile := range profiles {
		if !profile.SupportsReasoningEffort {
			continue
		}
		for _, effort := range profile.ReasoningEfforts {
			if id == normalizeModelID(profile.ModelID)+"-"+effort {
				return makeSpec(profile), effort, true
			}
		}
	}
	return ModelSpec{}, "", false
}

func ResolveModel(modelID string, catalogs ...[]modelcatalog.Profile) (ModelSpec, bool) {
	m, _, ok := ResolveModelAlias(modelID, catalogs...)
	return m, ok
}

// modelRoutedToCLI reports whether a resolved model uses Build CLI. The
// configured model table and dynamically discovered Build models explicitly
// carry UpstreamCLI; legacy config model lists no longer affect routing.
func modelRoutedToCLI(spec ModelSpec, _ *config.Config) bool { return spec.Upstream == UpstreamCLI }
