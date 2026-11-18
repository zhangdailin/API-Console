package warp

import (
	"bytes"
	"context"
	"encoding/json"
	"time"
)

func recoveryRead(ctx context.Context, bindings *bindingStore, local *recoveryStore, key string) ([]byte, error) {
	if bindings.redis != nil {
		backend, ok := bindings.redis.(redisBindings)
		if !ok || backend.store == nil || backend.store.RedisClient() == nil {
			return nil, errRecoveryUnavailable
		}
		raw, err := backend.store.RedisClient().Get(ctx, key).Bytes()
		if err != nil {
			return nil, errRecoveryUnavailable
		}
		return raw, nil
	}
	local.mu.Lock()
	defer local.mu.Unlock()
	return bytes.Clone(local.memory[key]), nil
}

// recoveryCAS deliberately does not fall back to local memory on Redis errors.
// Empty previous means create-only; a resume needs byte-exact current revision.
func recoveryCAS(ctx context.Context, bindings *bindingStore, local *recoveryStore, key string, previous, next []byte, attemptsKey, paidKey string, head recoveryHead) error {
	if bindings.redis != nil {
		backend, ok := bindings.redis.(redisBindings)
		if !ok || backend.store == nil || backend.store.RedisClient() == nil {
			return errRecoveryUnavailable
		}
		keys := []string{key}
		reserve := attemptsKey != ""
		if reserve {
			keys = append(keys, attemptsKey, paidKey)
		}
		result, err := backend.store.RedisClient().Eval(ctx, `
 local old=redis.call('GET',KEYS[1])
 if ARGV[1]=='' then if old then return 0 end
 elseif not old or old~=ARGV[1] then return 0 end
 if #KEYS==3 then
  local attempts=tonumber(redis.call('GET',KEYS[2]) or '0')
  local paid=tonumber(redis.call('GET',KEYS[3]) or '0')
  if not attempts or not paid or attempts<0 or paid<0 or attempts~=math.floor(attempts) or paid~=math.floor(paid) then return 0 end
  if paid<tonumber(ARGV[5]) then paid=tonumber(ARGV[5]) end
  if attempts>=tonumber(ARGV[4]) or paid>=tonumber(ARGV[6]) then return 0 end
  redis.call('SET',KEYS[2],attempts+1,'EX',ARGV[3])
  redis.call('SET',KEYS[3],paid+1,'EX',ARGV[3])
 end
 redis.call('SET',KEYS[1],ARGV[2],'EX',ARGV[3]); return 1
 `, keys, string(previous), string(next), int(recoveryTTL.Seconds()), recoveryAttemptLimit, head.PaidTurns, toolRoundLimit).Int()
		// head is the pre-reservation baseline passed by reserveRecovery.
		if err != nil || result != 1 {
			return errRecoveryUnavailable
		}
		return nil
	}
	local.mu.Lock()
	defer local.mu.Unlock()
	now := time.Now()
	for counter, until := range local.expires {
		if !until.After(now) {
			delete(local.expires, counter)
			delete(local.paid, counter)
			delete(local.attempts, counter)
		}
	}
	old := local.memory[key]
	if len(previous) == 0 {
		if len(old) > 0 {
			var existing recoveryHead
			if json.Unmarshal(old, &existing) != nil || time.Since(existing.SavedAt) <= recoveryTTL {
				return errRecoveryUnavailable
			}
		}
	} else if !bytes.Equal(old, previous) {
		return errRecoveryUnavailable
	}
	if attemptsKey != "" {
		paid := local.paid[paidKey]
		if paid < head.PaidTurns {
			paid = head.PaidTurns
		}
		if local.attempts[attemptsKey] >= recoveryAttemptLimit || paid >= toolRoundLimit {
			return errRecoveryUnavailable
		}
		local.attempts[attemptsKey]++
		local.paid[paidKey] = paid + 1
		local.expires[paidKey], local.expires[attemptsKey] = now.Add(recoveryTTL), now.Add(recoveryTTL)
	}
	if len(local.memory) >= bindingLimit && len(old) == 0 {
		for id, value := range local.memory {
			var existing recoveryHead
			if json.Unmarshal(value, &existing) != nil || time.Since(existing.SavedAt) > recoveryTTL {
				delete(local.memory, id)
			}
		}
		if len(local.memory) >= bindingLimit {
			return errRecoveryUnavailable
		}
	}
	local.memory[key] = bytes.Clone(next)
	return nil
}
