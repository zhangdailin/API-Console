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
	if a.DoubaoSessionUpdate != nil {
		patch := *a.DoubaoSessionUpdate
		copy.DoubaoSessionUpdate = &patch
	}
	copy.GrokModels = slices.Clone(a.GrokModels)
	copy.GrokModelCatalog = modelcatalog.CloneProfiles(a.GrokModelCatalog)
	copy.ModelCooldowns = maps.Clone(a.ModelCooldowns)
	copy.ModelCooldownReasons = maps.Clone(a.ModelCooldownReasons)
	copy.WorkBuddyModelIDs = slices.Clone(a.WorkBuddyModelIDs)
	copy.ClineModelIDs = slices.Clone(a.ClineModelIDs)
	copy.WarpModelIDs = slices.Clone(a.WarpModelIDs)
	copy.WarpModelWindows = maps.Clone(a.WarpModelWindows)
	copy.WarpModelVision = maps.Clone(a.WarpModelVision)
	copy.WarpQuota = a.WarpQuota.Clone()
	return &copy
}
