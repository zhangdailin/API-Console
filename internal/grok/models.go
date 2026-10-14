package grok

import (
	"strings"

	"orchids-api/internal/modelcatalog"
)

// ModelSpec defines one public model and how it maps to Grok upstream fields.
type ModelSpec struct {
	ID            string
	Name          string
	UpstreamModel string
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
		return ModelSpec{ID: normalizeModelID(p.ModelID), Name: p.ModelID, UpstreamModel: p.ModelID}
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
