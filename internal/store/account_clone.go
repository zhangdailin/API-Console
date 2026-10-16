package store

import (
	"maps"
	"orchids-api/internal/modelcatalog"
	"slices"
)

// Clone isolates mutable provider collections. Scalar snapshots and immutable
// credential strings can be copied without serialization.
func (acc *Account) Clone() *Account {
	if acc == nil {
		return nil
	}
	copy := *acc
	copy.GrokModels = slices.Clone(acc.GrokModels)
	copy.GrokModelCatalog = modelcatalog.CloneProfiles(acc.GrokModelCatalog)
	copy.ModelCooldowns = maps.Clone(acc.ModelCooldowns)
	copy.ModelCooldownReasons = maps.Clone(acc.ModelCooldownReasons)
	copy.WorkBuddyModelIDs = slices.Clone(acc.WorkBuddyModelIDs)
	copy.QoderModelIDs = slices.Clone(acc.QoderModelIDs)
	copy.QoderOrganizationTags = slices.Clone(acc.QoderOrganizationTags)
	copy.ClineModelIDs = slices.Clone(acc.ClineModelIDs)
	return &copy
}
