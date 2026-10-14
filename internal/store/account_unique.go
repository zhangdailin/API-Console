package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// CreateAccountUnlessDuplicate applies the caller's provider identity rules
// inside a Redis optimistic transaction. No credentials enter secondary keys.
func (s *Store) CreateAccountUnlessDuplicate(ctx context.Context, acc *Account, matches func(*Account) bool) (*Account, error) {
	backend, ok := s.accounts.(*redisStore)
	if !ok {
		return nil, fmt.Errorf("atomic account creation unavailable")
	}
	return backend.createAccountUnlessDuplicate(ctx, acc, matches)
}

func (s *redisStore) createAccountUnlessDuplicate(ctx context.Context, acc *Account, matches func(*Account) bool) (*Account, error) {
	if acc == nil || matches == nil {
		return nil, fmt.Errorf("account and identity matcher are required")
	}
	id, err := s.client.Incr(ctx, s.accountsNextIDKey()).Result()
	if err != nil {
		return nil, err
	}
	candidate := *acc
	candidate.ID = id
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = time.Now()
	}
	if candidate.UpdatedAt.IsZero() {
		candidate.UpdatedAt = time.Now()
	}
	data, err := s.marshalAccount(&candidate)
	if err != nil {
		return nil, err
	}
	var duplicate *Account
	for range 32 {
		duplicate = nil
		err = s.client.Watch(ctx, func(tx *redis.Tx) error {
			ids, err := tx.SMembers(ctx, s.accountsIDsKey()).Result()
			if err != nil {
				return err
			}
			keys := make([]string, len(ids))
			for i, text := range ids {
				id, err := strconv.ParseInt(text, 10, 64)
				if err != nil {
					return fmt.Errorf("invalid account identity in membership")
				}
				keys[i] = s.accountsKey(id)
			}
			// Watch records before reading: credential edits cannot race this scan.
			if len(keys) > 0 {
				if err := tx.Watch(ctx, keys...).Err(); err != nil {
					return err
				}
				pipe := tx.Pipeline()
				reads := make([]*redis.StringCmd, len(keys))
				for i, key := range keys {
					reads[i] = pipe.Get(ctx, key)
				}
				if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
					return err
				}
				for i, read := range reads {
					text, err := read.Result()
					if err == redis.Nil {
						continue
					}
					if err != nil {
						return err
					}
					id, _ := strconv.ParseInt(ids[i], 10, 64)
					existing, err := s.unmarshalAccount([]byte(text), id)
					if err != nil {
						return err
					}
					if matches(existing) {
						duplicate = existing
						return nil
					}
				}
			}
			// MULTI does not roll back command errors, so validate set types while
			// they are watched before publishing any record.
			for _, key := range []string{s.accountsIDsKey(), s.accountsEnabledKey()} {
				kind, err := tx.Type(ctx, key).Result()
				if err != nil {
					return err
				}
				if kind != "none" && kind != "set" {
					return fmt.Errorf("invalid account membership type")
				}
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, s.accountsKey(id), data, 0)
				pipeAccountMembership(ctx, pipe, s, id, candidate.Enabled)
				return nil
			})
			return err
		}, s.accountsIDsKey(), s.accountsEnabledKey(), s.accountsKey(id))
		if err != redis.TxFailedErr {
			break
		}
	}
	if err != nil || duplicate != nil {
		return duplicate, err
	}
	*acc = candidate
	s.publishChange(ctx, nil, id)
	return nil, nil
}
