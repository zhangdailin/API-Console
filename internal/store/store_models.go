package store

import (
	"strings"
)

// unsupportedGrokRoute keeps retired records in Redis for rollback without
// exposing them to model lookup or startup metadata repair.
func unsupportedGrokRoute(m *Model) bool {
	return m != nil && isUnsupportedModelRoute(m.Channel, m.Provider, m.ModelID)
}

func isUnsupportedModelRoute(channel, provider, modelID string) bool {
	return strings.EqualFold(strings.TrimSpace(channel), "grok") &&
		(!SupportsGrokProvider(provider) || strings.HasPrefix(strings.ToLower(strings.TrimSpace(modelID)), "console/"))
}
