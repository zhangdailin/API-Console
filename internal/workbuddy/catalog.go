package workbuddy

import (
	"strings"

	"encoding/json"
)

// catalogSnapshotRow preserves the observed catalog as JSON rows.
type catalogSnapshotRow struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	MaxInputTokens  int64  `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int64  `json:"max_output_tokens,omitempty"`
	Credits         string `json:"credits,omitempty"`
	SupportsImages  *bool  `json:"supports_images,omitempty"`
}

// CatalogSnapshot projects the observed catalog onto its stored form.
func CatalogSnapshot(models []WorkBuddyModel) []string {
	if len(models) == 0 {
		return nil
	}
	rows := make([]string, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		raw, err := json.Marshal(catalogSnapshotRow{
			ID:              id,
			Name:            strings.TrimSpace(model.Name),
			MaxInputTokens:  model.MaxInputTokens,
			MaxOutputTokens: model.MaxOutputTokens,
			Credits:         model.Credits,
			SupportsImages:  modelImageSupport(model),
		})
		if err != nil {
			continue
		}
		rows = append(rows, string(raw))
	}
	if len(rows) == 0 {
		return nil
	}
	return rows
}

func modelImageSupport(model WorkBuddyModel) *bool {
	if model.ImagesDisabled {
		return new(false)
	}
	return model.SupportsImages
}

// CatalogImageSupport projects only explicitly observed image support.
func CatalogImageSupport(rows []string) map[string]bool {
	var out map[string]bool
	for _, raw := range rows {
		var row catalogSnapshotRow
		if json.Unmarshal([]byte(raw), &row) != nil || row.SupportsImages == nil {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(row.ID))
		if id == "" {
			continue
		}
		if out == nil {
			out = make(map[string]bool)
		}
		prior, exists := out[id]
		out[id] = *row.SupportsImages && (!exists || prior)
	}
	return out
}

// CatalogContextWindows reads the windows back out of a stored snapshot.
//
// It answers two maps because the public model list reports the input window and
// the output budget separately. A row that carries neither is skipped, so
// "never observed" stays distinguishable from "observed as zero".
func CatalogContextWindows(ids []string) (input map[string]int, output map[string]int) {
	if len(ids) == 0 {
		return nil, nil
	}
	input = make(map[string]int, len(ids))
	output = make(map[string]int, len(ids))
	for _, raw := range ids {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		row := catalogSnapshotRow{}
		if !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(trimmed), &row) != nil {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(row.ID))
		if id == "" {
			continue
		}
		if row.MaxInputTokens > 0 {
			if existing, ok := input[id]; !ok || int(row.MaxInputTokens) > existing {
				input[id] = int(row.MaxInputTokens)
			}
		}
		if row.MaxOutputTokens > 0 {
			if existing, ok := output[id]; !ok || int(row.MaxOutputTokens) > existing {
				output[id] = int(row.MaxOutputTokens)
			}
		}
	}
	if len(input) == 0 {
		input = nil
	}
	if len(output) == 0 {
		output = nil
	}
	return input, output
}
