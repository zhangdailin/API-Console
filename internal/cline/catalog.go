package cline

import (
	"slices"
	"strings"

	"encoding/json"
)

// Model is one row of the recommended-models feed.
type Model struct {
	// ID is the upstream identifier, and the public identifier this gateway
	// publishes: a client that saw it in /v1/models must be able to ask for it
	// by that name.
	ID string `json:"id"`
	// Name is the upstream display name.
	Name string `json:"name"`
	// Provider is the half before the first "/" — the vendor the upstream
	// routes the identifier to.
	Provider string `json:"provider"`
	// RequiresStream reports whether the upstream only serves this identifier
	// on the streaming endpoint. The feed distinguishes the two by whether the
	// identifier carries a ":".
	RequiresStream bool `json:"requires_stream"`
}

// catalogSnapshotRow is the stored form of one catalog entry.
//
// The snapshot is stored as one JSON object per row, matching the other
// observed-catalog channels: the whitelist alone would throw away what it was
// observed alongside.
type catalogSnapshotRow struct {
	ID             string `json:"id"`
	Name           string `json:"name,omitempty"`
	Provider       string `json:"provider,omitempty"`
	RequiresStream bool   `json:"requires_stream,omitempty"`
}

// catalogPayload is the recommended-models feed. Only the free tier is
// published: the paid rows name models this account may not run.
type catalogPayload struct {
	Free []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"free"`
}

// parseCatalog reads the feed. An empty `free` list is an error rather than an
// empty catalog: publishing nothing would hide an upstream that stopped
// answering, and a caller cannot tell that apart from "this account has no
// models".
func parseCatalog(raw []byte) ([]Model, error) {
	var payload catalogPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(payload.Free))
	for _, entry := range payload.Free {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		provider := id
		if index := strings.Index(id, "/"); index >= 0 {
			provider = id[:index]
		}
		out = append(out, Model{
			ID:       id,
			Name:     strings.TrimSpace(entry.Name),
			Provider: provider,
			// The upstream serves a bare identifier only as a stream; an
			// identifier carrying ":" is a non-streaming variant.
			RequiresStream: !strings.Contains(id, ":"),
		})
	}
	return out, nil
}

// CatalogSnapshot projects the observed feed onto its stored form.
func CatalogSnapshot(models []Model) []string {
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
			ID:             id,
			Name:           strings.TrimSpace(model.Name),
			Provider:       strings.TrimSpace(model.Provider),
			RequiresStream: model.RequiresStream,
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

// CatalogSupportsModel reports whether an account-scoped snapshot advertised
// the requested model. Matching is case-insensitive but returns no entitlement
// for an empty or unreadable snapshot.
func CatalogSupportsModel(rows []string, modelID string) bool {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return true
	}
	return slices.ContainsFunc(rows, func(row string) bool { return strings.EqualFold(catalogID(row), modelID) })
}

// catalogID accepts current JSON snapshot rows only.
func catalogID(raw string) string {
	var row catalogSnapshotRow
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") || json.Unmarshal([]byte(raw), &row) != nil {
		return ""
	}
	return strings.TrimSpace(row.ID)
}
