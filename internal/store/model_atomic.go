package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// saveModelAtomic publishes the row, indexes and channel default in one commit.
// Watching all rows also coordinates with catalog reconciliation and deletes.
func (s *redisStore) saveModelAtomic(ctx context.Context, m *Model) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	for range 32 {
		err = s.client.Watch(ctx, func(tx *redis.Tx) error {
			for key, expected := range map[string]string{s.modelsIDsKey(): "set", s.modelsChannelModelIDMapKey(): "hash"} {
				kind, err := tx.Type(ctx, key).Result()
				if err != nil {
					return err
				}
				if kind != "none" && kind != expected {
					return fmt.Errorf("invalid model index type")
				}
			}
			ids, err := tx.SMembers(ctx, s.modelsIDsKey()).Result()
			if err != nil {
				return err
			}
			keys := []string{s.modelsKey(m.ID)}
			for _, id := range ids {
				keys = append(keys, s.modelsKey(id))
			}
			if err := tx.Watch(ctx, keys...).Err(); err != nil {
				return err
			}
			updates := map[string][]byte{m.ID: data}
			var previous *Model
			allIDs := append(ids, m.ID)
			pipe := tx.Pipeline()
			reads := make([]*redis.StringCmd, len(allIDs))
			for i, id := range allIDs {
				reads[i] = pipe.Get(ctx, s.modelsKey(id))
			}
			if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
				return err
			}
			for i, id := range allIDs {
				raw, err := reads[i].Bytes()
				if err == redis.Nil {
					continue
				}
				if err != nil {
					return err
				}
				var row Model
				if err := json.Unmarshal(raw, &row); err != nil {
					return fmt.Errorf("invalid stored model: %w", err)
				}
				if id == m.ID {
					previous = &row
				} else if m.IsDefault && row.IsDefault && normalizeModelChannelKey(row.Channel) == normalizeModelChannelKey(m.Channel) {
					row.IsDefault = false
					updates[id], err = json.Marshal(&row)
					if err != nil {
						return err
					}
				}
			}
			oldIndex := ""
			if previous != nil && previous.ModelID != "" {
				key := modelChannelIndexKey(previous.Channel, previous.ModelID)
				if key != modelChannelIndexKey(m.Channel, m.ModelID) {
					owner, err := tx.HGet(ctx, s.modelsChannelModelIDMapKey(), key).Result()
					if err != nil && err != redis.Nil {
						return err
					}
					if owner == m.ID {
						oldIndex = key
					}
				}
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				for id, row := range updates {
					pipe.Set(ctx, s.modelsKey(id), row, 0)
				}
				pipe.SAdd(ctx, s.modelsIDsKey(), m.ID)
				if oldIndex != "" {
					pipe.HDel(ctx, s.modelsChannelModelIDMapKey(), oldIndex)
				}
				if m.ModelID != "" {
					pipe.HSet(ctx, s.modelsChannelModelIDMapKey(), modelChannelIndexKey(m.Channel, m.ModelID), m.ID)
				}
				return nil
			})
			return err
		}, s.modelsIDsKey(), s.modelsChannelModelIDMapKey())
		if err != redis.TxFailedErr {
			return err
		}
	}
	return err
}
