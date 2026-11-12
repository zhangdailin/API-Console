package store

import (
	"maps"
	"orchids-api/internal/modelcatalog"
	"slices"
)

// Clone isolates mutable provider collections. Scalar snapshots and immutable
// credential strings can be copied without serialization.
func (a *Account) Clone() *Account {
	if a == nil {
		return nil
	}
	copy := *a
	copy.GrokModels = slices.Clone(a.GrokModels)
	copy.GrokModelCatalog = modelcatalog.CloneProfiles(a.GrokModelCatalog)
	copy.GrokConsoleModels = slices.Clone(a.GrokConsoleModels)
	copy.ModelCooldowns = maps.Clone(a.ModelCooldowns)
	copy.ModelCooldownReasons = maps.Clone(a.ModelCooldownReasons)
	copy.WorkBuddyModelIDs = slices.Clone(a.WorkBuddyModelIDs)
	copy.ClineModelIDs = slices.Clone(a.ClineModelIDs)
	return &copy
}
