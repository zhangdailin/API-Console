package modelpolicy

import (
	"slices"
	"strings"
)

// reasoningEffortCapabilities maps an upstream model slug to the effort levels
// its wire contract accepts. A slug that is absent falls back to "none" only,
// so an unknown model never advertises a level upstream may reject.
var reasoningEffortCapabilities = map[string][]string{
	"grok-4.5":               {"low", "medium", "high"},
	"grok-4.6":               {"low", "medium", "high", "xhigh"},
	"grok-4.7":               {"low", "medium", "high", "xhigh"},
	"grok-composer-2.5-fast": {"none"},
}

// GrokModelSlug strips the optional Build provider prefix.
func GrokModelSlug(publicID string) string {
	slug := strings.ToLower(strings.TrimSpace(publicID))
	slug = strings.TrimPrefix(slug, "build/")
	return slug
}

// IsGrokComposerModel reports whether the model belongs to the Composer family,
// which never accepts a configurable reasoning effort.
func IsGrokComposerModel(publicID string) bool {
	return strings.HasPrefix(GrokModelSlug(publicID), "grok-composer-")
}

// SupportedReasoningEfforts returns the configurable effort levels for a model.
// An empty result means the model reasons intrinsically but exposes no
// configurable effort parameter.
func SupportedReasoningEfforts(publicID string) []string {
	slug := GrokModelSlug(publicID)
	if levels, ok := reasoningEffortCapabilities[slug]; ok {
		return append([]string(nil), levels...)
	}
	return []string{"none"}
}

// SupportsReasoningEffort reports whether the model accepts the given level.
func SupportsReasoningEffort(publicID, effort string) bool {
	effort = strings.ToLower(strings.TrimSpace(effort))
	return slices.ContainsFunc(SupportedReasoningEfforts(publicID), func(level string) bool { return level == effort })
}
