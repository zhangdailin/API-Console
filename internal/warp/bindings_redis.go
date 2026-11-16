package warp

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"orchids-api/internal/store"
)

// redisBindings adapts the shared Redis client to the binding store. Records
// contain scoped, expiring continuation state, never account credentials. Task
// history may contain private user content and must not be logged or exposed.
type redisBindings struct{ store *store.Store }

func (r redisBindings) SetBinding(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if r.store == nil {
		return nil
	}
	client := r.store.RedisClient()
	if client == nil {
		return nil
	}
	if ok, err := client.SetNX(ctx, key, value, ttl).Result(); err != nil {
		return err
	} else if ok {
		return nil
	}
	raw, err := client.Get(ctx, key).Bytes()
	if err != nil {
		return err
	}
	var old, next callBinding
	if json.Unmarshal(raw, &old) != nil || json.Unmarshal(value, &next) != nil || !sameBindingResponse(old, next) {
		return errors.New("Warp continuation call identity conflict")
	}
	return nil
}

func (r redisBindings) GetBinding(ctx context.Context, key string) ([]byte, error) {
	if r.store == nil {
		return nil, nil
	}
	client := r.store.RedisClient()
	if client == nil {
		return nil, nil
	}
	raw, err := client.Get(ctx, key).Bytes()
	if err != nil {
		return nil, err
	}
	return raw, nil
}
