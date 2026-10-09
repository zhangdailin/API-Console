package workbuddy

import (
	"strings"

	"encoding/json"
)

// freeModelIDs is the WorkBuddy free tier confirmed by the operator and by
// production use after the metered package is spent. The upstream /v3/config
// feed exposes model availability but no billing flag, so this deliberately
// small allowlist is safer than guessing from names such as "flash".
var freeModelIDs = map[string]struct{}{
	"deepseek-v4.1-flash":    {},
	"deepseek-v4.1-flash-sg": {},
	"glm-5.3-flash":          {},
	"hy4-preview-f":          {},
	"hy3":                    {},
}

// IsFreeModel reports whether WorkBuddy currently treats an advertised model as
// free. Aliases and unknown catalog rows are not widened automatically.
func IsFreeModel(modelID string) bool {
	_, ok := freeModelIDs[strings.ToLower(strings.TrimSpace(modelID))]
	return ok
}

// IsFreeModelInCatalog requires both the confirmed free entitlement and this
// account's latest advertised catalog. This prevents an account from receiving a
// model it cannot actually serve merely because another account advertised it.
func IsFreeModelInCatalog(ids []string, modelID string) bool {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if !IsFreeModel(modelID) {
		return false
	}
	for _, raw := range ids {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		row := catalogSnapshotRow{}
		if !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(trimmed), &row) != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(row.ID), modelID) {
			return true
		}
	}
	return false
}
