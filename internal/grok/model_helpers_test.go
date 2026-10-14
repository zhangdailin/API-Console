package grok

import (
	"orchids-api/internal/modelcatalog"
)

// Convenience adapters for catalog contract tests. Production resolves models
// through the enabled account catalogs and ResolveModelAlias.
func ResolveModel(modelID string, catalogs ...[]modelcatalog.Profile) (ModelSpec, bool) {
	m, _, ok := ResolveModelAlias(modelID, catalogs...)
	return m, ok
}
