package loadbalancer

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Monitoring must preserve a failed read instead of using selection's zero-load
// fallback. Selection continues to use GetCounts and atomic TryAcquire.
func (t *MemoryConnTracker) GetCountsChecked(ctx context.Context, ids []int64) (map[int64]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return t.GetCounts(ids), nil
}

func (t *RedisConnTracker) GetCountsChecked(ctx context.Context, ids []int64) (map[int64]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	counts := make(map[int64]int64, len(ids))
	if len(ids) == 0 {
		return counts, ctx.Err()
	}
	pipe := t.client.Pipeline()
	now := fmt.Sprint(time.Now().UnixMilli())
	commands := make([]*redis.IntCmd, len(ids))
	for i, id := range ids {
		key := t.key(id)
		pipe.ZRemRangeByScore(ctx, key, "-inf", now)
		commands[i] = pipe.ZCard(ctx, key)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	for i, command := range commands {
		value, err := command.Result()
		if err != nil {
			return nil, err
		}
		counts[ids[i]] = value
	}
	return counts, nil
}
