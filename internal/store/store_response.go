package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

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
	// ToolNamespaces restores client tool identities from native upstream names.
	// It shares the response's owner and TTL, including across gateway replicas.
	ToolNamespaces json.RawMessage `json:"tool_namespaces,omitempty"`
	// PreviousResponseID links a continuation to the response it continued, so
	// the stored input list can report the whole conversation instead of only
	// the last turn.
	PreviousResponseID string    `json:"previous_response_id,omitempty"`
	ExpiresAt          time.Time `json:"expires_at"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
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

type responseStore interface {
	SaveStoredResponse(ctx context.Context, response *StoredResponse, ttl time.Duration) error
	GetStoredResponse(ctx context.Context, responseID, ownerHash string) (*StoredResponse, error)
	DeleteStoredResponse(ctx context.Context, responseID, ownerHash string) error
}

type reasoningReplayStore interface {
	DeleteReasoningReplay(ctx context.Context, model, sessionKey string) error
	SaveReasoningReplay(ctx context.Context, replay *StoredReasoningReplay, ttl time.Duration) error
	GetReasoningReplay(ctx context.Context, model, sessionKey string) (*StoredReasoningReplay, error)
	SaveSessionAffinity(ctx context.Context, affinity *StoredSessionAffinity, ttl time.Duration) error
	GetSessionAffinity(ctx context.Context, provider, model, sessionKey string) (*StoredSessionAffinity, error)
}

func (s *Store) SaveStoredResponse(ctx context.Context, response *StoredResponse, ttl time.Duration) error {
	if s == nil || s.responses == nil {
		return fmt.Errorf("response store not configured")
	}
	return s.responses.SaveStoredResponse(ctx, response, ttl)
}

func (s *Store) GetStoredResponse(ctx context.Context, responseID, ownerHash string) (*StoredResponse, error) {
	if s == nil || s.responses == nil {
		return nil, fmt.Errorf("response store not configured")
	}
	return s.responses.GetStoredResponse(ctx, responseID, ownerHash)
}

func (s *Store) DeleteStoredResponse(ctx context.Context, responseID, ownerHash string) error {
	if s == nil || s.responses == nil {
		return fmt.Errorf("response store not configured")
	}
	return s.responses.DeleteStoredResponse(ctx, responseID, ownerHash)
}

func (s *Store) DeleteReasoningReplay(ctx context.Context, model, key string) error {
	if s == nil || s.reasoning == nil {
		return nil
	}
	return s.reasoning.DeleteReasoningReplay(ctx, model, key)
}

func (s *Store) SaveReasoningReplay(ctx context.Context, replay *StoredReasoningReplay, ttl time.Duration) error {
	if s == nil || s.reasoning == nil {
		return fmt.Errorf("reasoning replay store not configured")
	}
	return s.reasoning.SaveReasoningReplay(ctx, replay, ttl)
}

func (s *Store) GetReasoningReplay(ctx context.Context, model, sessionKey string) (*StoredReasoningReplay, error) {
	if s == nil || s.reasoning == nil {
		return nil, fmt.Errorf("reasoning replay store not configured")
	}
	return s.reasoning.GetReasoningReplay(ctx, model, sessionKey)
}

func (s *Store) SaveSessionAffinity(ctx context.Context, affinity *StoredSessionAffinity, ttl time.Duration) error {
	if s == nil || s.reasoning == nil {
		return fmt.Errorf("session affinity store not configured")
	}
	return s.reasoning.SaveSessionAffinity(ctx, affinity, ttl)
}

func (s *Store) GetSessionAffinity(ctx context.Context, provider, model, sessionKey string) (*StoredSessionAffinity, error) {
	if s == nil || s.reasoning == nil {
		return nil, fmt.Errorf("session affinity store not configured")
	}
	return s.reasoning.GetSessionAffinity(ctx, provider, model, sessionKey)
}
