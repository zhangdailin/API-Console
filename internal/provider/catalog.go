package provider

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/store"
)

// Model is one catalog entry as a channel publishes it.
type Model struct {
	ID   string
	Name string
	// Provider and UpstreamModel are route metadata a feed may name next to the
	// identifier. They stay empty for a channel whose feed names neither.
	Provider      string
	UpstreamModel string
	// BillingTier is upstream metadata, never a name-based guess. Only "free"
	// grants routing to an account whose metered allowance is exhausted.
	BillingTier   string
	BillingSource string
}

// Observation is the result of one upstream catalog read for one account.
type Observation struct {
	Models []Model
	// Record writes the observation onto the account in memory and reports
	// whether it changed anything. An empty observation records nothing: the
	// account keeps its last known good snapshot. Persisting is the caller's
	// decision.
	Record func(acc *store.Account, now time.Time) bool
}

// CatalogKind says where the rows a discovery publishes come from.
type CatalogKind int

const (
	// CatalogRows publishes the models of the observation itself and falls
	// back to the stored snapshot only when the read fails.
	CatalogRows CatalogKind = iota
	// CatalogAccountView records the observation on the account first and
	// always publishes what the account then stores.
	CatalogAccountView
)

// Catalog is how a channel's model catalog is read.
//
// Every row a caller publishes from it has to be an observation of an upstream
// catalog for a currently active account. Nothing here returns a compiled-in or
// defaulted list.
type Catalog struct {
	// Source labels the discovery report ("qoder_upstream_models").
	Source string
	Kind   CatalogKind
	// RequestTimeoutSeconds caps the request timeout of a manual discovery
	// read, so one slow account cannot hold the admin request open. Zero keeps
	// the configured timeout.
	RequestTimeoutSeconds int
	// Eligible narrows the enabled accounts of the channel to the ones a
	// catalog read can use. Nil means every enabled account.
	Eligible func(acc *store.Account) bool
	// Observe performs the upstream read. It does not modify the account.
	Observe func(ctx context.Context, env Env, acc *store.Account) (Observation, error)
	// LastKnown projects the snapshot already stored on the account.
	LastKnown func(acc *store.Account) []Model
	// SetSnapshot stores a raw snapshot on the account in memory. It is nil
	// for a channel whose snapshot is not a list of opaque rows.
	SetSnapshot func(acc *store.Account, rows []string, now time.Time)
	// ShapeRecord fills in what the channel knows about every model row it
	// publishes (routing defaults, capabilities). Nil leaves the row as
	// discovery built it.
	ShapeRecord func(record *store.Model)
	// ProviderScope, when set, makes a complete round prune only the rows of
	// this provider plane instead of the whole channel.
	ProviderScope string
}

// RecordSnapshot returns the Record function of a channel whose snapshot is a
// list of opaque rows.
func RecordSnapshot(rows []string, set func(acc *store.Account, rows []string, now time.Time)) func(*store.Account, time.Time) bool {
	return func(acc *store.Account, now time.Time) bool {
		if acc == nil || len(rows) == 0 || set == nil {
			return false
		}
		set(acc, rows, now)
		return true
	}
}

// PersistSnapshot records a raw catalog snapshot on the account and saves it.
// An empty snapshot is not an observation and leaves the stored one untouched.
// Callers supply their own failure context so manual discovery and the
// background refresh keep distinct log messages.
func PersistSnapshot(ctx context.Context, s *store.Store, acc *store.Account, catalog *Catalog, rows []string, updateFailureMessage string) {
	if acc == nil || catalog == nil || catalog.SetSnapshot == nil || len(rows) == 0 {
		return
	}
	catalog.SetSnapshot(acc, rows, time.Now())
	if err := s.UpdateAccount(ctx, acc); err != nil {
		slog.Warn(updateFailureMessage, "account_id", acc.ID, "error", err)
	}
}

// SnapshotRow is the part of a stored catalog row every row-snapshot channel
// writes. A channel reads the fields its own feed carries and ignores the rest.
type SnapshotRow struct {
	ID          string   `json:"id"`
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Provider    string   `json:"provider"`
	PriceFactor *float64 `json:"price_factor"`
}

// ParseSnapshotRows decodes the JSON rows of a stored snapshot. A row that is
// not a JSON object — a bare identifier written by an older build — is skipped
// rather than guessed at.
func ParseSnapshotRows(rows []string) []SnapshotRow {
	out := make([]SnapshotRow, 0, len(rows))
	for _, raw := range rows {
		trimmed := strings.TrimSpace(raw)
		if !strings.HasPrefix(trimmed, "{") {
			continue
		}
		row := SnapshotRow{}
		if json.Unmarshal([]byte(trimmed), &row) != nil {
			continue
		}
		out = append(out, row)
	}
	return out
}
