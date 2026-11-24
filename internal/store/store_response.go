package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const DefaultStoredResponseTTL = 30 * 24 * time.Hour

// StoredResponse records the ownership needed to continue or manage an
// upstream Responses resource without retaining the request or response body.
type StoredResponse struct {
	ResponseID     string `json:"response_id"`
	OwnerHash      string `json:"owner_hash"`
	AccountID      int64  `json:"account_id"`
	Model          string `json:"model"`
	Provider       string `json:"provider"`
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	ContentType    string `json:"content_type,omitempty"`
	Body           []byte `json:"body,omitempty"`
	// InputItems is the request input the response was created from, normalized
	// to the Responses item shape. GET /responses/{id}/input_items serves it
	// back. Records written before this field existed simply report an empty
	// list rather than failing, and previous_response_id expansion is unaffected
	// because it reads Body.
	InputItems json.RawMessage `json:"input_items,omitempty"`
	// InputFileRefs is an internal storage sidecar; reads restore InputItems.
	InputFileRefs         []responseInputFileRef `json:"input_file_refs,omitempty"`
	ExternalizeInputFiles bool                   `json:"-"`
	// ToolNamespaces restores client tool identities from native upstream names.
	// It shares the response's owner and TTL, including across gateway replicas.
	ToolNamespaces json.RawMessage `json:"tool_namespaces,omitempty"`
	// PreviousResponseID links a continuation to the response it continued, so
	// the stored input list can report the whole conversation instead of only
	// the last turn.
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	// InputItemsDelta distinguishes Build turn-only input from historical snapshots.
	InputItemsDelta bool      `json:"input_items_delta,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

var (
	ErrStoredResponseIdentityRequired = errors.New("response id and owner are required")
	ErrStoredResponseChanged          = errors.New("stored response changed")
)

// prepareStoredResponse normalizes the identity, timestamps, and stored inputs.
func prepareStoredResponse(response *StoredResponse, now time.Time, ttl time.Duration) (StoredResponse, map[string]string, error) {
	if response == nil {
		return StoredResponse{}, nil, ErrStoredResponseIdentityRequired
	}
	row := *response
	row.ResponseID, row.OwnerHash = strings.TrimSpace(row.ResponseID), strings.TrimSpace(row.OwnerHash)
	if row.ResponseID == "" || row.OwnerHash == "" {
		return StoredResponse{}, nil, ErrStoredResponseIdentityRequired
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	row.UpdatedAt, row.ExpiresAt = now, now.Add(ttl)
	files, err := detachResponseInputFiles(&row)
	return row, files, err
}

func (r *StoredResponse) bodyMatches(previous *StoredResponse) bool {
	return r.UpdatedAt.Equal(previous.UpdatedAt) && bytes.Equal(r.Body, previous.Body)
}

// StoredReasoningReplay contains normalized portable replay items. The key
// is already tenant/model/session isolated by the gateway; Redis persistence
// lets later turns resume on another replica without storing plaintext chain
// of thought.
type StoredReasoningReplay struct {
	Model      string            `json:"model"`
	SessionKey string            `json:"session_key"`
	Items      []json.RawMessage `json:"items,omitempty"`
	ExpiresAt  time.Time         `json:"expires_at"`
}

type StoredSessionAffinity struct {
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	SessionKey string    `json:"session_key"`
	AccountID  int64     `json:"account_id"`
	ExpiresAt  time.Time `json:"expires_at"`
}
