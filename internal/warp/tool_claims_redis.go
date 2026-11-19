package warp

import (
	"context"
	"fmt"
	"time"
)

func (r redisBindings) ClaimToolTurn(ctx context.Context, counter string, claims []string, ttl time.Duration) error {
	client := r.store.RedisClient()
	if client == nil {
		return fmt.Errorf("Warp continuation claim store unavailable")
	}
	keys := append([]string{counter}, claims...)
	result, err := client.Eval(ctx, `
 for i=2,#KEYS do if redis.call('EXISTS',KEYS[i])==1 then return -1 end end
 local count=tonumber(redis.call('GET',KEYS[1]) or '0')
 if not count or count<0 or count~=math.floor(count) then return -2 end
 for i=2,#KEYS do redis.call('SET',KEYS[i],'1','EX',ARGV[1]) end
 redis.call('SET',KEYS[1],count+1,'EX',ARGV[1]); return 1
 `, keys, int(ttl.Seconds())).Int()
	if err != nil || result != 1 {
		return fmt.Errorf("Warp tool result already consumed or claim store unavailable")
	}
	return nil
}
