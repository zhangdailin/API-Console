package grok

import (
	"orchids-api/internal/config"
	"orchids-api/internal/modelcatalog"
)

// Convenience adapters for catalog contract tests. Production resolves models
// through the enabled account catalogs and ResolveModelAlias.
func ResolveModel(modelID string, catalogs ...[]modelcatalog.Profile) (ModelSpec, bool) {
	m, _, ok := ResolveModelAlias(modelID, catalogs...)
	return m, ok
}

// modelRoutedToCLI reports whether a resolved model uses Build CLI. The
// resolved model must explicitly carry UpstreamCLI; config model names cannot
// grant a route.
func modelRoutedToCLI(spec ModelSpec, _ *config.Config) bool { return spec.Upstream == UpstreamCLI }
