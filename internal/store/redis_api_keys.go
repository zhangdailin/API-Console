package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"encoding/json"
	"github.com/redis/go-redis/v9"
)

type apiKeyRecord struct {
	EncryptedSecret      string   `json:"encrypted_secret,omitempty"`
	ID                   int64    `json:"id"`
	Name                 string   `json:"name"`
	KeyHash              string   `json:"key_hash"`
	KeyPrefix            string   `json:"key_prefix"`
	KeySuffix            string   `json:"key_suffix"`
	Enabled              bool     `json:"enabled"`
	AllowedModels        []string `json:"allowed_models,omitempty"`
	RPMLimit             int      `json:"rpm_limit,omitempty"`
	MaxConcurrent        int      `json:"max_concurrent,omitempty"`
	BillingLimitUSDTicks int64    `json:"billing_limit_usd_ticks,omitempty"`
	BillingUsedUSDTicks  int64    `json:"billing_used_usd_ticks,omitempty"`
	BillingPeriodDays    int      `json:"billing_period_days,omitempty"`
	// BillingPeriodStartedAt travels with the record: without it a restart would
	// forget when the current period began and never roll over.
	BillingPeriodStartedAt time.Time  `json:"billing_period_started_at,omitempty"`
	ExpiresAt              *time.Time `json:"expires_at,omitempty"`
	LastUsedAt             *time.Time `json:"last_used_at"`
	CreatedAt              time.Time  `json:"created_at"`
}

func (s *redisStore) CreateApiKey(ctx context.Context, key *ApiKey) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}

	id, err := s.client.Incr(ctx, s.apiKeysNextIDKey()).Result()
	if err != nil {
		return err
	}

	now := time.Now()
	key.ID = id
	if key.CreatedAt.IsZero() {
		key.CreatedAt = now
	}

	record := apiKeyRecordFromKey(key)
	if key.KeyFull != "" {
		record.EncryptedSecret, err = s.sealApiKey(key.KeyFull)
		if err != nil {
			return err
		}
		key.SecretAvailable = true
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}

	pipe := s.client.TxPipeline()
	pipe.Set(ctx, s.apiKeysKey(id), data, 0)
	pipe.SAdd(ctx, s.apiKeysIDsKey(), id)
	if record.KeyHash != "" {
		pipe.Set(ctx, s.apiKeysHashKey(record.KeyHash), id, 0)
	}
	// The limit is mirrored next to the reservations so the atomic reserve script
	// does not need to parse the key record. BillingUsedUSDTicks only seeds the
	// counter; the counter is authoritative afterwards.
	pipe.Set(ctx, s.apiKeyBillingLimitKey(id), record.BillingLimitUSDTicks, 0)
	if record.BillingUsedUSDTicks > 0 {
		pipe.Set(ctx, s.apiKeyBillingUsedKey(id), record.BillingUsedUSDTicks, 0)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) ListApiKeys(ctx context.Context) ([]*ApiKey, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	ids, err := s.client.SMembers(ctx, s.apiKeysIDsKey()).Result()
	if err != nil {
		return nil, err
	}
	return s.getApiKeysByIDs(ctx, ids)
}

func (s *redisStore) UpdateApiKey(ctx context.Context, key *ApiKey) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if key == nil || key.ID == 0 {
		return ErrNoRows
	}
	return s.mutateApiKey(ctx, key.ID, func(current *apiKeyRecord) error {
		next := apiKeyRecordFromKey(key)
		// Secret-bearing rows may only change identity through atomic rotation.
		if current.EncryptedSecret != "" {
			next.KeyHash, next.KeyPrefix, next.KeySuffix = current.KeyHash, current.KeyPrefix, current.KeySuffix
		}
		next.EncryptedSecret = current.EncryptedSecret
		*current = next
		return nil
	})
}

func (s *redisStore) DeleteApiKey(ctx context.Context, id int64) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return ErrNoRows
	}
	key, err := s.getApiKeyByID(ctx, id)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Del(ctx, s.apiKeysKey(id))
	pipe.SRem(ctx, s.apiKeysIDsKey(), id)
	if key.KeyHash != "" {
		pipe.Del(ctx, s.apiKeysHashKey(key.KeyHash))
	}
	pipe.Del(ctx, s.apiKeyBillingReservationsKey(id), s.apiKeyBillingUsedKey(id), s.apiKeyBillingSettledKey(id), s.apiKeyBillingLimitKey(id))
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) GetApiKeyByID(ctx context.Context, id int64) (*ApiKey, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	return s.getApiKeyByID(ctx, id)
}

func (s *redisStore) GetApiKeyByHash(ctx context.Context, hash string) (*ApiKey, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return nil, ErrNoRows
	}
	s.keyIndexMu.RLock()
	cachedID := s.keyIndex[hash]
	s.keyIndexMu.RUnlock()
	if cachedID != 0 {
		key, err := s.getApiKeyByID(ctx, cachedID)
		if err == nil && key.KeyHash == hash {
			return key, nil
		}
		if err != nil && err != ErrNoRows {
			return nil, err
		}
		s.keyIndexMu.Lock()
		delete(s.keyIndex, hash)
		s.keyIndexMu.Unlock()
	}
	id, err := s.client.Get(ctx, s.apiKeysHashKey(hash)).Int64()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	key, err := s.getApiKeyByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if key.KeyHash != hash {
		return nil, ErrNoRows
	}
	s.keyIndexMu.Lock()
	if s.keyIndex == nil || len(s.keyIndex) >= 4096 {
		s.keyIndex = make(map[string]int64)
	}
	s.keyIndex[hash] = id
	s.keyIndexMu.Unlock()
	return key, nil
}

func (s *redisStore) ConsumeApiKeyRPM(ctx context.Context, id int64, limit int, now time.Time) (bool, error) {
	if s == nil || s.client == nil {
		return false, fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return false, ErrNoRows
	}
	minute := now.UTC().Unix() / 60
	ttl := int64(120)
	count, err := consumeApiKeyRPMScript.Run(
		ctx,
		s.client,
		[]string{s.apiKeyRPMKey(id, minute), s.apiKeysKey(id)},
		ttl,
		now.UTC().Format(time.RFC3339Nano),
		limit,
	).Int64()
	if err != nil {
		return false, err
	}
	return count <= int64(limit), nil
}

func (s *redisStore) TouchApiKeyLastUsed(ctx context.Context, id int64, now time.Time) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return ErrNoRows
	}
	return touchApiKeyLastUsedScript.Run(ctx, s.client, []string{s.apiKeysKey(id)}, now.UTC().Format(time.RFC3339Nano)).Err()
}

// ReserveApiKeyBilling holds amount ticks of a key's spending limit until
// expiresAt. The check and the insert are one Lua script so two replicas cannot
// both admit a request that the limit only covers once.
func (s *redisStore) ReserveApiKeyBilling(ctx context.Context, id int64, eventID string, amount int64, expiresAt time.Time) (bool, error) {
	if s == nil || s.client == nil {
		return false, fmt.Errorf("redis store not configured")
	}
	if id == 0 || strings.TrimSpace(eventID) == "" {
		return false, ErrNoRows
	}
	if amount <= 0 {
		return false, fmt.Errorf("billing reservation amount must be positive")
	}
	if expiresAt.IsZero() {
		return false, fmt.Errorf("billing reservation expiry is required")
	}
	now := time.Now().UTC()
	reserved, err := reserveApiKeyBillingScript.Run(
		ctx,
		s.client,
		[]string{s.apiKeyBillingReservationsKey(id), s.apiKeyBillingUsedKey(id), s.apiKeyBillingLimitKey(id)},
		eventID,
		amount,
		now.Unix(),
		expiresAt.UTC().Unix(),
	).Int64()
	if err != nil {
		return false, err
	}
	return reserved == 1, nil
}

// SettleApiKeyBilling charges actual usage: the hold for eventID is dropped and
// amount ticks are added to the settled counter. An unknown event id is still
// charged, because the request really ran and an expired hold must not erase it.
func (s *redisStore) SettleApiKeyBilling(ctx context.Context, id int64, eventID string, amount int64) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 || strings.TrimSpace(eventID) == "" {
		return ErrNoRows
	}
	if amount < 0 {
		return fmt.Errorf("billing settlement amount must not be negative")
	}
	result, err := settleApiKeyBillingScript.Run(
		ctx,
		s.client,
		[]string{s.apiKeyBillingReservationsKey(id), s.apiKeyBillingUsedKey(id), s.apiKeyBillingSettledKey(id)},
		eventID,
		amount,
	).Int64()
	if err != nil {
		return err
	}
	if result < 0 {
		return fmt.Errorf("billing event %q was already settled with a different amount", eventID)
	}
	return nil
}

// ReleaseApiKeyBilling drops a hold without charging it and reports whether one
// was held, which is what stops a settling request from being charged twice.
func (s *redisStore) ReleaseApiKeyBilling(ctx context.Context, id int64, eventID string) (bool, error) {
	if s == nil || s.client == nil {
		return false, fmt.Errorf("redis store not configured")
	}
	if id == 0 || strings.TrimSpace(eventID) == "" {
		return false, ErrNoRows
	}
	released, err := releaseApiKeyBillingScript.Run(
		ctx,
		s.client,
		[]string{s.apiKeyBillingReservationsKey(id)},
		eventID,
	).Int64()
	if err != nil {
		return false, err
	}
	return released == 1, nil
}

// ResetApiKeyBilling zeroes the settled counter and drops every pending hold.
// The configured limit itself is untouched.
func (s *redisStore) ResetApiKeyBilling(ctx context.Context, id int64) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return ErrNoRows
	}
	_, err := resetApiKeyBillingScript.Run(
		ctx,
		s.client,
		[]string{s.apiKeyBillingReservationsKey(id), s.apiKeyBillingUsedKey(id), s.apiKeyBillingSettledKey(id)},
	).Int64()
	return err
}

func (s *redisStore) RolloverApiKeyBilling(ctx context.Context, key *ApiKey, now time.Time) (bool, error) {
	if s == nil || s.client == nil {
		return false, fmt.Errorf("redis store not configured")
	}
	if key == nil || key.ID == 0 {
		return false, ErrNoRows
	}
	if key.BillingPeriodDays <= 0 {
		return false, nil
	}
	now = now.UTC()
	started := key.BillingPeriodStartedAt.UTC()
	if !started.IsZero() && now.Before(started.AddDate(0, 0, key.BillingPeriodDays)) {
		return false, nil
	}
	expected := ""
	if !started.IsZero() {
		expected = started.Format(time.RFC3339Nano)
	}
	result, err := rolloverApiKeyBillingScript.Run(
		ctx,
		s.client,
		[]string{s.apiKeysKey(key.ID), s.apiKeyBillingReservationsKey(key.ID), s.apiKeyBillingUsedKey(key.ID), s.apiKeyBillingSettledKey(key.ID)},
		expected,
		now.Format(time.RFC3339Nano),
		key.BillingPeriodDays,
	).Int64()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func (s *redisStore) getApiKeyByID(ctx context.Context, id int64) (*ApiKey, error) {
	if id == 0 {
		return nil, ErrNoRows
	}
	// One round trip: the record and the settled-usage counter that projects into
	// it. The counter, not the stored JSON, is the source of truth for usage.
	pipe := s.client.Pipeline()
	recordCmd := pipe.Get(ctx, s.apiKeysKey(id))
	usedCmd := pipe.Get(ctx, s.apiKeyBillingUsedKey(id))
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}
	value, err := recordCmd.Result()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	var record apiKeyRecord
	if err := json.Unmarshal([]byte(value), &record); err != nil {
		return nil, err
	}
	key := record.toApiKey()
	if key.ID == 0 {
		key.ID = id
	}
	if used, err := usedCmd.Int64(); err == nil {
		key.BillingUsedUSDTicks = used
	}
	return key, nil
}

func (s *redisStore) getApiKeysByIDs(ctx context.Context, ids []string) ([]*ApiKey, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	idNums := parseSortedInt64s(ids)
	if len(idNums) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(idNums))
	usedKeys := make([]string, 0, len(idNums))
	for _, id := range idNums {
		keys = append(keys, s.apiKeysKey(id))
		usedKeys = append(usedKeys, s.apiKeyBillingUsedKey(id))
	}

	pipe := s.client.Pipeline()
	recordsCmd := pipe.MGet(ctx, keys...)
	usedCmd := pipe.MGet(ctx, usedKeys...)
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}
	values, err := recordsCmd.Result()
	if err != nil {
		return nil, err
	}
	usedValues, _ := usedCmd.Result()

	results := make([]*ApiKey, len(values))
	decode := func(i int) {
		strVal, ok := values[i].(string)
		if !ok || strVal == "" {
			return
		}
		var record apiKeyRecord
		if err := json.Unmarshal([]byte(strVal), &record); err != nil {
			return
		}
		key := record.toApiKey()
		if key.ID == 0 {
			key.ID = idNums[i]
		}
		if i < len(usedValues) {
			if raw, ok := usedValues[i].(string); ok {
				if used, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
					key.BillingUsedUSDTicks = used
				}
			}
		}
		results[i] = key
	}
	forEachIndex(len(values), decode)

	return compactNonNil(results), nil
}

func (s *redisStore) apiKeysKey(id int64) string {
	return s.prefix + "api_keys:id:" + strconv.FormatInt(id, 10)
}

func (s *redisStore) apiKeysIDsKey() string { return s.prefix + "api_keys:ids" }

func (s *redisStore) apiKeysNextIDKey() string { return s.prefix + "api_keys:next_id" }

func (s *redisStore) apiKeysHashKey(hash string) string { return s.prefix + "api_keys:hash:" + hash }

func (s *redisStore) apiKeyRPMKey(id, minute int64) string {
	return s.prefix + "api_keys:rpm:" + strconv.FormatInt(id, 10) + ":" + strconv.FormatInt(minute, 10)
}

// apiKeyBillingReservationsKey holds the live holds of one key as a hash of
// "eventID -> <amount ticks>:<expiry unix seconds>". Expired fields are pruned
// by the reserve script, and the hash itself expires shortly after its last hold.
func (s *redisStore) apiKeyBillingReservationsKey(id int64) string {
	return s.prefix + "keybilling:res:" + strconv.FormatInt(id, 10)
}

// apiKeyBillingUsedKey is the settled usage counter in ticks. It is the source
// of truth for how much a key has spent.
func (s *redisStore) apiKeyBillingUsedKey(id int64) string {
	return s.prefix + "keybilling:used:" + strconv.FormatInt(id, 10)
}

func (s *redisStore) apiKeyBillingSettledKey(id int64) string {
	return s.prefix + "keybilling:settled:" + strconv.FormatInt(id, 10)
}

// apiKeyBillingLimitKey mirrors the key's billing limit so the reserve script
// can decide without loading and parsing the key record.
func (s *redisStore) apiKeyBillingLimitKey(id int64) string {
	return s.prefix + "keybilling:limit:" + strconv.FormatInt(id, 10)
}

func apiKeyRecordFromKey(key *ApiKey) apiKeyRecord {
	return apiKeyRecord{
		ID:      key.ID,
		Name:    key.Name,
		KeyHash: key.KeyHash,
		// KeyFull is deliberately never persisted; it is only returned once by
		// the create endpoint before this record reaches Redis.
		KeyPrefix:              key.KeyPrefix,
		KeySuffix:              key.KeySuffix,
		Enabled:                key.Enabled,
		AllowedModels:          append([]string(nil), key.AllowedModels...),
		RPMLimit:               key.RPMLimit,
		MaxConcurrent:          key.MaxConcurrent,
		BillingLimitUSDTicks:   key.BillingLimitUSDTicks,
		BillingUsedUSDTicks:    key.BillingUsedUSDTicks,
		BillingPeriodDays:      key.BillingPeriodDays,
		BillingPeriodStartedAt: key.BillingPeriodStartedAt,
		ExpiresAt:              key.ExpiresAt,
		LastUsedAt:             key.LastUsedAt,
		CreatedAt:              key.CreatedAt,
	}
}

func (r apiKeyRecord) toApiKey() *ApiKey {
	return &ApiKey{
		SecretAvailable:        r.EncryptedSecret != "",
		ID:                     r.ID,
		Name:                   r.Name,
		KeyHash:                r.KeyHash,
		KeyPrefix:              r.KeyPrefix,
		KeySuffix:              r.KeySuffix,
		Enabled:                r.Enabled,
		AllowedModels:          append([]string(nil), r.AllowedModels...),
		RPMLimit:               r.RPMLimit,
		MaxConcurrent:          r.MaxConcurrent,
		BillingLimitUSDTicks:   r.BillingLimitUSDTicks,
		BillingUsedUSDTicks:    r.BillingUsedUSDTicks,
		BillingPeriodDays:      r.BillingPeriodDays,
		BillingPeriodStartedAt: r.BillingPeriodStartedAt,
		ExpiresAt:              r.ExpiresAt,
		LastUsedAt:             r.LastUsedAt,
		CreatedAt:              r.CreatedAt,
	}
}
