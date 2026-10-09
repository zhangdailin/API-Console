package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/redis/go-redis/v9"
)

// digestKey names an opaque Redis key after a hash of its composite identity, so
// caller-supplied text (response ids, session keys) never reaches the key space.
func (s *redisStore) digestKey(namespace string, parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return s.prefix + namespace + hex.EncodeToString(digest[:])
}

func (s *redisStore) storedResponseKey(responseID, ownerHash string) string {
	return s.digestKey("responses:ownership:", strings.TrimSpace(ownerHash), strings.TrimSpace(responseID))
}

func (s *redisStore) SaveStoredResponse(ctx context.Context, response *StoredResponse, ttl time.Duration) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if response == nil || strings.TrimSpace(response.ResponseID) == "" || strings.TrimSpace(response.OwnerHash) == "" {
		return fmt.Errorf("response id and owner are required")
	}
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	now := time.Now().UTC()
	stored := *response
	stored.ResponseID = strings.TrimSpace(stored.ResponseID)
	stored.OwnerHash = strings.TrimSpace(stored.OwnerHash)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = now
	}
	stored.UpdatedAt = now
	stored.ExpiresAt = now.Add(ttl)
	data, err := json.Marshal(&stored)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.storedResponseKey(stored.ResponseID, stored.OwnerHash), data, ttl).Err()
}

func (s *redisStore) GetStoredResponse(ctx context.Context, responseID, ownerHash string) (*StoredResponse, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	key := s.storedResponseKey(responseID, ownerHash)
	return getExpiringRedisJSON[StoredResponse](ctx, s, key, func(response *StoredResponse) time.Time {
		return response.ExpiresAt
	})
}

func getExpiringRedisJSON[T any](ctx context.Context, s *redisStore, key string, expiresAt func(*T) time.Time) (*T, error) {
	value, err := s.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	var result T
	if err := json.Unmarshal(value, &result); err != nil {
		return nil, err
	}
	expiry := expiresAt(&result)
	if !expiry.IsZero() && !time.Now().UTC().Before(expiry) {
		_ = s.client.Del(ctx, key).Err()
		return nil, ErrNoRows
	}
	return &result, nil
}

func (s *redisStore) DeleteStoredResponse(ctx context.Context, responseID, ownerHash string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	deleted, err := s.client.Del(ctx, s.storedResponseKey(responseID, ownerHash)).Result()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrNoRows
	}
	return nil
}

func (s *redisStore) reasoningReplayKey(model, sessionKey string) string {
	return s.digestKey("grok:reasoning_replay:", strings.ToLower(strings.TrimSpace(model)), strings.TrimSpace(sessionKey))
}

func (s *redisStore) DeleteReasoningReplay(ctx context.Context, model, key string) error {
	return s.client.Del(ctx, s.reasoningReplayKey(model, key)).Err()
}

func (s *redisStore) SaveReasoningReplay(ctx context.Context, replay *StoredReasoningReplay, ttl time.Duration) error {
	if replay == nil || strings.TrimSpace(replay.Model) == "" || strings.TrimSpace(replay.SessionKey) == "" || len(replay.Items) == 0 {
		return fmt.Errorf("invalid reasoning replay")
	}
	// Replay readers decode each item into a map; JSON scalars, arrays and null
	// cannot be replayed even though they are otherwise valid JSON values.
	for _, item := range replay.Items {
		trimmed := strings.TrimSpace(string(item))
		if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(item) {
			return fmt.Errorf("invalid reasoning replay item")
		}
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	next := *replay
	next.ExpiresAt = time.Now().UTC().Add(ttl)
	raw, err := json.Marshal(&next)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.reasoningReplayKey(next.Model, next.SessionKey), raw, ttl).Err()
}

func (s *redisStore) GetReasoningReplay(ctx context.Context, model, sessionKey string) (*StoredReasoningReplay, error) {
	key := s.reasoningReplayKey(model, sessionKey)
	replay, err := getExpiringRedisJSON[StoredReasoningReplay](ctx, s, key, func(replay *StoredReasoningReplay) time.Time {
		return replay.ExpiresAt
	})
	if err != nil || replay == nil {
		return replay, err
	}
	if len(replay.Items) == 0 {
		return nil, nil
	}
	for _, item := range replay.Items {
		var object map[string]interface{}
		if json.Unmarshal(item, &object) != nil || object == nil {
			return nil, nil
		}
	}
	return replay, nil
}

func (s *redisStore) sessionAffinityKey(provider, model, sessionKey string) string {
	return s.digestKey("grok:session_affinity:",
		strings.ToLower(strings.TrimSpace(provider)),
		strings.ToLower(strings.TrimSpace(model)),
		strings.TrimSpace(sessionKey))
}

func (s *redisStore) SaveSessionAffinity(ctx context.Context, affinity *StoredSessionAffinity, ttl time.Duration) error {
	if affinity == nil || strings.TrimSpace(affinity.Provider) == "" || strings.TrimSpace(affinity.Model) == "" || strings.TrimSpace(affinity.SessionKey) == "" || affinity.AccountID == 0 {
		return fmt.Errorf("invalid session affinity")
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	next := *affinity
	next.ExpiresAt = time.Now().UTC().Add(ttl)
	raw, err := json.Marshal(&next)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.sessionAffinityKey(next.Provider, next.Model, next.SessionKey), raw, ttl).Err()
}

func (s *redisStore) GetSessionAffinity(ctx context.Context, provider, model, sessionKey string) (*StoredSessionAffinity, error) {
	key := s.sessionAffinityKey(provider, model, sessionKey)
	return getExpiringRedisJSON[StoredSessionAffinity](ctx, s, key, func(affinity *StoredSessionAffinity) time.Time {
		return affinity.ExpiresAt
	})
}
