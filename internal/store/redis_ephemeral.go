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
func (s *Store) digestKey(namespace string, parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return s.prefix + namespace + hex.EncodeToString(digest[:])
}

func (s *Store) storedResponseKey(responseID, ownerHash string) string {
	return s.digestKey("responses:ownership:", strings.TrimSpace(ownerHash), strings.TrimSpace(responseID))
}

func (s *Store) responseInputFileKey(owner, digest string) string {
	return s.digestKey("responses:input-file:", owner, digest)
}

var holdResponseInputFile = redis.NewScript(`
	local left = redis.call("PTTL", KEYS[1])
	if left == -2 then
		if not ARGV[2] then return 0 end
		redis.call("SET", KEYS[1], ARGV[2], "PX", ARGV[1])
	elseif left >= 0 and left < tonumber(ARGV[1]) then
		redis.call("PEXPIRE", KEYS[1], ARGV[1])
	end
	return 1
`)

func (s *Store) SaveStoredResponse(ctx context.Context, response *StoredResponse, ttl time.Duration) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if ttl <= 0 {
		ttl = DefaultStoredResponseTTL
	}
	stored, files, err := prepareStoredResponse(response, time.Now().UTC(), ttl)
	if err != nil {
		return err
	}
	for digest, payload := range files {
		keys := []string{s.responseInputFileKey(stored.OwnerHash, digest)}
		fileTTL := (ttl + time.Minute).Milliseconds()
		exists, err := holdResponseInputFile.Run(ctx, s.client, keys, fileTTL).Int()
		if err != nil {
			return err
		}
		if exists == 0 {
			if err := holdResponseInputFile.Run(ctx, s.client, keys, fileTTL, payload).Err(); err != nil {
				return err
			}
		}
	}
	return s.setExpiringJSON(ctx, s.storedResponseKey(stored.ResponseID, stored.OwnerHash), &stored, ttl)
}

func (s *Store) GetStoredResponse(ctx context.Context, responseID, ownerHash string) (*StoredResponse, error) {
	row, err := s.GetStoredResponseMetadata(ctx, responseID, ownerHash)
	if err != nil {
		return nil, err
	}
	if err := restoreResponseInputFiles(row, func(digest string) (string, error) {
		payload, err := s.client.Get(ctx, s.responseInputFileKey(strings.TrimSpace(ownerHash), digest)).Result()
		if err == redis.Nil {
			return "", errInvalidResponseInputFile
		}
		return payload, err
	}); err != nil {
		return nil, err
	}
	return row, nil
}

// GetStoredResponseMetadata reads a response without resolving stored input files.
func (s *Store) GetStoredResponseMetadata(ctx context.Context, responseID, ownerHash string) (*StoredResponse, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	key := s.storedResponseKey(responseID, ownerHash)
	return getExpiringRedisJSON[StoredResponse](ctx, s, key, func(response *StoredResponse) time.Time {
		return response.ExpiresAt
	})
}

// UpdateStoredResponseBody changes the representation without rewriting inputs or TTL.
func (s *Store) UpdateStoredResponseBody(ctx context.Context, previous *StoredResponse, body []byte) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if previous == nil || previous.ResponseID == "" || previous.OwnerHash == "" {
		return ErrNoRows
	}
	key := s.storedResponseKey(previous.ResponseID, previous.OwnerHash)
	err := s.client.Watch(ctx, func(tx *redis.Tx) error {
		raw, err := tx.Get(ctx, key).Bytes()
		if err == redis.Nil {
			return ErrNoRows
		}
		if err != nil {
			return err
		}
		var row StoredResponse
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		now := time.Now().UTC()
		if !row.ExpiresAt.IsZero() && !row.ExpiresAt.After(now) {
			return ErrNoRows
		}
		if !row.bodyMatches(previous) {
			return ErrStoredResponseChanged
		}
		row.Body, row.UpdatedAt = body, now
		raw, err = json.Marshal(row)
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.SetArgs(ctx, key, raw, redis.SetArgs{Mode: "XX", TTL: redis.KeepTTL})
			return nil
		})
		return err
	}, key)
	if err == redis.TxFailedErr {
		return ErrStoredResponseChanged
	}
	if err == redis.Nil {
		return ErrNoRows
	}
	return err
}

func (s *Store) setExpiringJSON(ctx context.Context, key string, value any, ttl time.Duration) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, key, raw, ttl).Err()
}

func getExpiringRedisJSON[T any](ctx context.Context, s *Store, key string, expiresAt func(*T) time.Time) (*T, error) {
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

func (s *Store) DeleteStoredResponse(ctx context.Context, responseID, ownerHash string) error {
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

func (s *Store) reasoningReplayKey(model, sessionKey string) string {
	return s.digestKey("grok:reasoning_replay:", strings.ToLower(strings.TrimSpace(model)), strings.TrimSpace(sessionKey))
}

func (s *Store) DeleteReasoningReplay(ctx context.Context, model, key string) error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Del(ctx, s.reasoningReplayKey(model, key)).Err()
}

func (s *Store) SaveReasoningReplay(ctx context.Context, replay *StoredReasoningReplay, ttl time.Duration) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("reasoning replay store not configured")
	}
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
	return s.setExpiringJSON(ctx, s.reasoningReplayKey(next.Model, next.SessionKey), &next, ttl)
}

func (s *Store) GetReasoningReplay(ctx context.Context, model, sessionKey string) (*StoredReasoningReplay, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("reasoning replay store not configured")
	}
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
		var object map[string]any
		if json.Unmarshal(item, &object) != nil || object == nil {
			return nil, nil
		}
	}
	return replay, nil
}

func (s *Store) sessionAffinityKey(provider, model, sessionKey string) string {
	return s.digestKey("grok:session_affinity:",
		strings.ToLower(strings.TrimSpace(provider)),
		strings.ToLower(strings.TrimSpace(model)),
		strings.TrimSpace(sessionKey))
}

func (s *Store) SaveSessionAffinity(ctx context.Context, affinity *StoredSessionAffinity, ttl time.Duration) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("session affinity store not configured")
	}
	if affinity == nil || strings.TrimSpace(affinity.Provider) == "" || strings.TrimSpace(affinity.Model) == "" || strings.TrimSpace(affinity.SessionKey) == "" || affinity.AccountID == 0 {
		return fmt.Errorf("invalid session affinity")
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	next := *affinity
	next.ExpiresAt = time.Now().UTC().Add(ttl)
	return s.setExpiringJSON(ctx, s.sessionAffinityKey(next.Provider, next.Model, next.SessionKey), &next, ttl)
}

func (s *Store) GetSessionAffinity(ctx context.Context, provider, model, sessionKey string) (*StoredSessionAffinity, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("session affinity store not configured")
	}
	key := s.sessionAffinityKey(provider, model, sessionKey)
	return getExpiringRedisJSON[StoredSessionAffinity](ctx, s, key, func(affinity *StoredSessionAffinity) time.Time {
		return affinity.ExpiresAt
	})
}
