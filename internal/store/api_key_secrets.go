package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

var ErrApiKeySecretUnavailable = fmt.Errorf("rotate this legacy API key before copying it")

func (s *Store) GetApiKeySecret(ctx context.Context, id int64) (string, error) {
	return s.apiKeys.GetApiKeySecret(ctx, id)
}

func (s *Store) RotateApiKey(ctx context.Context, id int64, secret string) (*ApiKey, error) {
	return s.apiKeys.RotateApiKey(ctx, id, secret)
}

func (s *Store) PatchApiKey(ctx context.Context, id int64, fields map[string]json.RawMessage) error {
	return s.apiKeys.PatchApiKey(ctx, id, fields)
}

func (s *redisStore) sealApiKey(secret string) (string, error) {
	if s.keySecrets == nil {
		return "", fmt.Errorf("API key encryption is unavailable")
	}
	return s.keySecrets.encrypt(secret)
}

func (s *redisStore) GetApiKeySecret(ctx context.Context, id int64) (string, error) {
	raw, err := s.client.Get(ctx, s.apiKeysKey(id)).Bytes()
	if err == redis.Nil {
		return "", ErrNoRows
	}
	if err != nil {
		return "", err
	}
	var row apiKeyRecord
	if err = json.Unmarshal(raw, &row); err != nil {
		return "", err
	}
	if row.EncryptedSecret == "" {
		return "", ErrApiKeySecretUnavailable
	}
	if !strings.HasPrefix(row.EncryptedSecret, encryptedCredentialPrefix) {
		return "", fmt.Errorf("API key ciphertext is invalid")
	}
	if s.keySecrets == nil {
		return "", fmt.Errorf("API key encryption is unavailable")
	}
	secret, err := s.keySecrets.decrypt(row.EncryptedSecret)
	if err != nil {
		return "", fmt.Errorf("API key decryption failed")
	}
	digest := sha256.Sum256([]byte(secret))
	if hex.EncodeToString(digest[:]) != row.KeyHash {
		return "", fmt.Errorf("API key secret integrity check failed")
	}
	return secret, nil
}

// mutateApiKey retries against the current row, preserving concurrent policy,
// billing-period and secret changes. Row, hash indexes and limit mirror commit together.
func (s *redisStore) mutateApiKey(ctx context.Context, id int64, change func(*apiKeyRecord) error) error {
	for attempt := 0; attempt < 8; attempt++ {
		err := s.client.Watch(ctx, func(tx *redis.Tx) error {
			raw, err := tx.Get(ctx, s.apiKeysKey(id)).Bytes()
			if err == redis.Nil {
				return ErrNoRows
			}
			if err != nil {
				return err
			}
			var row apiKeyRecord
			if err = json.Unmarshal(raw, &row); err != nil {
				return err
			}
			oldHash := row.KeyHash
			if err = change(&row); err != nil {
				return err
			}
			data, err := json.Marshal(row)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, s.apiKeysKey(id), data, 0)
				if oldHash != row.KeyHash {
					pipe.Del(ctx, s.apiKeysHashKey(oldHash))
					pipe.Set(ctx, s.apiKeysHashKey(row.KeyHash), id, 0)
				}
				pipe.Set(ctx, s.apiKeyBillingLimitKey(id), row.BillingLimitUSDTicks, 0)
				return nil
			})
			return err
		}, s.apiKeysKey(id))
		if err != redis.TxFailedErr {
			return err
		}
	}
	return fmt.Errorf("API key changed concurrently; retry the operation")
}

func (s *redisStore) RotateApiKey(ctx context.Context, id int64, secret string) (*ApiKey, error) {
	if len(secret) < 7 {
		return nil, fmt.Errorf("invalid API key secret")
	}
	sealed, err := s.sealApiKey(secret)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(secret))
	var committed apiKeyRecord
	err = s.mutateApiKey(ctx, id, func(row *apiKeyRecord) error {
		row.KeyHash = hex.EncodeToString(digest[:])
		row.EncryptedSecret = sealed
		row.KeyPrefix = "sk-"
		row.KeySuffix = secret[len(secret)-4:]
		committed = *row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return committed.toApiKey(), nil
}

func (s *redisStore) PatchApiKey(ctx context.Context, id int64, fields map[string]json.RawMessage) error {
	allowed := map[string]bool{"enabled": true, "allowed_models": true, "rpm_limit": true, "max_concurrent": true, "expires_at": true, "billing_limit_usd_ticks": true, "billing_period_days": true}
	for name := range fields {
		if !allowed[name] {
			return fmt.Errorf("unsupported API key policy field")
		}
	}
	return s.mutateApiKey(ctx, id, func(row *apiKeyRecord) error {
		raw, err := json.Marshal(row)
		if err != nil {
			return err
		}
		var current map[string]json.RawMessage
		if err = json.Unmarshal(raw, &current); err != nil {
			return err
		}
		for name, value := range fields {
			current[name] = value
		}
		raw, err = json.Marshal(current)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, row)
	})
}
