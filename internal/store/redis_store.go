package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/util"

	"github.com/redis/go-redis/v9"
)

type redisStore struct {
	client      *redis.Client
	prefix      string
	credentials *credentialCipher
	// changeEmitter announces persisted account mutations. It is nil when nobody
	// listens (tests, a store without the notification bus), and a nil emitter is
	// simply silent rather than an error.
	changeMu      sync.Mutex
	changeEmitter ChangeEmitter
	changePending map[int64]AccountChange
	changeWake    chan struct{}
	changeStop    chan struct{}
	changeDone    chan struct{}
	changeOnce    sync.Once
	changeClosed  bool
}

// ChangeEmitter receives one notification per persisted account mutation. The
// store stays unaware of the subscribers behind it.
type ChangeEmitter interface {
	Publish(change AccountChange)
}

// AccountChange is the store's own description of a mutation. It is defined here
// (rather than imported) so the store keeps no dependency on the notification
// package; the bus adapts it. Current is left for the emitter to resolve, so the
// store never blocks a write on a subscriber.
type AccountChange struct {
	AccountID int64
	Previous  *Account
	Origin    string
}

const AccountChangeOriginScheduler = "token_refresh_scheduler"

type accountChangeOriginContextKey struct{}

// WithAccountChangeOrigin marks writes made by an internal controller. Cache
// invalidation still happens, while a filtered scheduler subscriber can ignore
// its own persisted result.
func WithAccountChangeOrigin(ctx context.Context, origin string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, accountChangeOriginContextKey{}, strings.TrimSpace(origin))
}

func accountChangeOrigin(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	origin, _ := ctx.Value(accountChangeOriginContextKey{}).(string)
	return strings.TrimSpace(origin)
}

// SetChangeEmitter wires the notification target. Passing nil disables it.
func (s *redisStore) SetChangeEmitter(emitter ChangeEmitter) {
	if s == nil {
		return
	}
	s.changeMu.Lock()
	s.changeEmitter = emitter
	s.changeMu.Unlock()
}

// publishChange coalesces pending mutations by account and wakes one dispatcher.
// This preserves the write path's non-blocking contract without creating one
// goroutine per API request. Keeping the earliest Previous value means a burst of
// writes is classified against the state before the burst, which is sufficient
// for every cache/status subscriber while bounding memory by account count.
func (s *redisStore) publishChange(ctx context.Context, previous *Account, id int64) {
	if s == nil || id == 0 {
		return
	}
	var previousCopy *Account
	if previous != nil {
		copied := *previous
		previousCopy = &copied
	}
	change := AccountChange{AccountID: id, Previous: previousCopy, Origin: accountChangeOrigin(ctx)}

	s.changeMu.Lock()
	if s.changeEmitter == nil || s.changePending == nil || s.changeClosed {
		s.changeMu.Unlock()
		return
	}
	if existing, ok := s.changePending[id]; ok {
		// Preserve the state before the first mutation in the coalesced burst, but
		// keep the newest origin for scheduler feedback suppression.
		existing.Origin = change.Origin
		s.changePending[id] = existing
	} else {
		s.changePending[id] = change
	}
	s.changeMu.Unlock()
	select {
	case s.changeWake <- struct{}{}:
	default:
	}
}

func (s *redisStore) dispatchChanges() {
	defer close(s.changeDone)
	for {
		select {
		case <-s.changeWake:
			s.flushChanges()
		case <-s.changeStop:
			s.flushChanges()
			return
		}
	}
}

func (s *redisStore) flushChanges() {
	for {
		s.changeMu.Lock()
		if len(s.changePending) == 0 {
			s.changeMu.Unlock()
			return
		}
		var change AccountChange
		for id, pending := range s.changePending {
			change = pending
			delete(s.changePending, id)
			break
		}
		emitter := s.changeEmitter
		s.changeMu.Unlock()
		if emitter == nil {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("Account change emitter panicked", "error", r)
				}
			}()
			emitter.Publish(change)
		}()
	}
}

const redisBatchParallelThreshold = 32

var (
	consumeApiKeyRPMScript = redis.NewScript(`
		local value = redis.call("GET", KEYS[2])
		if not value then return redis.error_reply("api key not found") end
		local api_key = cjson.decode(value)
		api_key.last_used_at = ARGV[2]
		redis.call("SET", KEYS[2], cjson.encode(api_key))
		local limit = tonumber(ARGV[3])
		if limit <= 0 then return 0 end
		local count = redis.call("INCR", KEYS[1])
		if count == 1 then
			redis.call("EXPIRE", KEYS[1], tonumber(ARGV[1]))
		end
		return count
	`)
	touchApiKeyLastUsedScript = redis.NewScript(`
		local value = redis.call("GET", KEYS[1])
		if not value then return redis.error_reply("api key not found") end
		local api_key = cjson.decode(value)
		api_key.last_used_at = ARGV[1]
		redis.call("SET", KEYS[1], cjson.encode(api_key))
		return 1
	`)
	incrementAccountStatsScript = redis.NewScript(`
		local key = KEYS[1]
		local usage = tonumber(ARGV[1])
		local count = tonumber(ARGV[2])
		local now_str = ARGV[3]
		local today = ARGV[4]
		local operation_id = ARGV[5]
		if operation_id ~= "" then
			if redis.call("SISMEMBER", KEYS[2], operation_id) == 1 then return "DUPLICATE" end
		end
		local val = redis.call("GET", key)
		if not val then return redis.error_reply("account not found") end
		local acc = cjson.decode(val)
		local acc_type = ""
		if acc.account_type ~= nil then
			acc_type = string.lower(tostring(acc.account_type))
		end
		if acc_type ~= "grok" and acc_type ~= "qoder" and acc_type ~= "workbuddy" then
			acc.usage_current = (acc.usage_current or 0) + usage
		end
		acc.usage_total = (acc.usage_total or 0) + usage
		-- The daily figure is rolled here rather than by a scheduled job: a
		-- request observed on a different date than the one recorded starts a
		-- new day. A total that never resets cannot answer how much of the
		-- upstream rate limit this account has spent today.
		if acc.tokens_date == nil or acc.tokens_date < today then
			acc.tokens_date = today
			acc.tokens_today = usage
		elseif acc.tokens_date == today then
			acc.tokens_today = (acc.tokens_today or 0) + usage
		end
		acc.request_count = (acc.request_count or 0) + count
		acc.last_used_at = now_str
		acc.updated_at = now_str
		redis.call("SET", key, cjson.encode(acc))
		if operation_id ~= "" then
			redis.call("SADD", KEYS[2], operation_id)
			redis.call("EXPIRE", KEYS[2], 691200)
		end
		return "OK"
	`)
	// reserveApiKeyBillingScript implements the whole reservation decision in one
	// atomic step: expired holds are pruned, the live holds are summed with the
	// settled counter, and the new hold is only inserted when the key's limit
	// still covers it. A repeat of the same event id with the same amount is
	// idempotent; the same event id with a different amount is a conflict.
	// KEYS: reservations hash, used counter, limit mirror.
	// ARGV: event id, amount ticks, now unix seconds, expiry unix seconds.
	reserveApiKeyBillingScript = redis.NewScript(`
		local event_id = ARGV[1]
		local amount = tonumber(ARGV[2])
		local now = tonumber(ARGV[3])
		local expires_at = tonumber(ARGV[4])
		local limit = tonumber(redis.call("GET", KEYS[3]) or "0") or 0
		local used = tonumber(redis.call("GET", KEYS[2]) or "0") or 0
		local live_sum = 0
		local max_expiry = expires_at
		local fields = redis.call("HGETALL", KEYS[1])
		for i = 1, #fields, 2 do
			local field = fields[i]
			local raw = fields[i + 1]
			local separator = string.find(raw, ":", 1, true)
			local held = nil
			local expiry = nil
			if separator then
				held = tonumber(string.sub(raw, 1, separator - 1))
				expiry = tonumber(string.sub(raw, separator + 1))
			end
			if held == nil or expiry == nil or expiry <= now then
				redis.call("HDEL", KEYS[1], field)
			elseif field == event_id then
				if held == amount then return 1 end
				return redis.error_reply("billing reservation exists with a different amount")
			else
				live_sum = live_sum + held
				if expiry > max_expiry then max_expiry = expiry end
			end
		end
		if limit > 0 and used + live_sum + amount > limit then return 0 end
		redis.call("HSET", KEYS[1], event_id, tostring(amount) .. ":" .. tostring(expires_at))
		redis.call("PEXPIREAT", KEYS[1], max_expiry * 1000 + 60000)
		return 1
	`)
	// settleApiKeyBillingScript books usage exactly once per event id. Recording
	// the event in the same Lua transaction makes a retry after an ambiguous
	// network timeout safe: Redis either applied all three mutations or none.
	settleApiKeyBillingScript = redis.NewScript(`
		local prior = redis.call("HGET", KEYS[3], ARGV[1])
		if prior then
			if prior ~= tostring(ARGV[2]) then return -1 end
			return 0
		end
		redis.call("HSET", KEYS[3], ARGV[1], tostring(ARGV[2]))
		redis.call("HDEL", KEYS[1], ARGV[1])
		redis.call("INCRBY", KEYS[2], tonumber(ARGV[2]))
		return 1
	`)
	releaseApiKeyBillingScript = redis.NewScript(`
		return redis.call("HDEL", KEYS[1], ARGV[1])
	`)
	resetApiKeyBillingScript = redis.NewScript(`
		redis.call("DEL", KEYS[1])
		redis.call("SET", KEYS[2], 0)
		redis.call("DEL", KEYS[3])
		return 1
	`)
	// rolloverApiKeyBillingScript compares and mutates the durable API-key row in
	// one Redis transaction. It deliberately preserves KEYS[2] (live holds), so a
	// request admitted before rollover can still settle or release normally.
	rolloverApiKeyBillingScript = redis.NewScript(`
		local raw = redis.call("GET", KEYS[1])
		if not raw then return redis.error_reply("api key not found") end
		local row = cjson.decode(raw)
		if (tonumber(row.billing_period_days or "0") or 0) ~= tonumber(ARGV[3]) then return 0 end
		local current_start = tostring(row.billing_period_started_at or "")
		if ARGV[1] == "" then
			if current_start ~= "" and string.sub(current_start, 1, 5) ~= "0001-" then return 0 end
		elseif current_start ~= ARGV[1] then
			return 0
		end
		row.billing_period_started_at = ARGV[2]
		row.updated_at = ARGV[2]
		redis.call("SET", KEYS[1], cjson.encode(row))
		redis.call("SET", KEYS[3], 0)
		redis.call("DEL", KEYS[4])
		return 1
	`)
	listModelsScript = redis.NewScript(`
		local ids = redis.call("SMEMBERS", KEYS[1])
		local rows = {}
		for _, id in ipairs(ids) do
			local value = redis.call("GET", ARGV[1] .. id)
			if value then table.insert(rows, value) end
		end
		return rows
	`)
	reconcileDiscoveredModelsScript = redis.NewScript(`
		local row_prefix, channel = ARGV[1], ARGV[2]
		local prune, incoming, provider_scope = ARGV[3] == "1", cjson.decode(ARGV[4]), string.lower(tostring(ARGV[5] or ""))
		local wanted, existing = {}, {}
		local added, updated, deleted, protected = {}, {}, {}, {}
		for _, id in ipairs(redis.call("SMEMBERS", KEYS[1])) do
			local raw = redis.call("GET", row_prefix .. id)
			if raw then
				local row = cjson.decode(raw)
				local row_channel = string.lower(tostring(row.channel or ""))
				row_channel = string.gsub(string.gsub(row_channel, "_", "-"), " ", "-")
				if row_channel == channel and row.model_id then existing[tostring(row.model_id)] = {id=id,row=row} end
			end
		end
		for _, row in ipairs(incoming) do
			local current_incoming = row
			local model_id = tostring(row.model_id)
			wanted[model_id] = true
			local current = existing[model_id]
			if current then
				local origin = string.lower(tostring(current.row.origin or ""))
				if (provider_scope ~= "" and prune) or origin == "discovery" then
					-- An authoritative scoped catalog owns every row in its plane,
					-- including older admin/catalog rows. Non-pruning refreshes retain
					-- the operator-field protection below.
					row.id, row.created_at = current.id, current.row.created_at or row.created_at
				else
					-- Manual/config/legacy rows are never replaced or pruned. A matching
					-- observation may only promote verification and fill metadata that
					-- the operator never supplied.
					row = current.row
					row.verified = true
					if (not row.name or tostring(row.name) == "" or tostring(row.name) == model_id) and current_incoming.name then row.name = current_incoming.name end
					if (not row.provider or tostring(row.provider) == "") and current_incoming.provider then row.provider = current_incoming.provider end
					if (not row.upstream_model or tostring(row.upstream_model) == "") and current_incoming.upstream_model then row.upstream_model = current_incoming.upstream_model end
					if current_incoming.billing_tier ~= nil then row.billing_tier = current_incoming.billing_tier end
					if current_incoming.billing_source ~= nil then row.billing_source = current_incoming.billing_source end
					table.insert(protected, model_id)
				end
				redis.call("SET", row_prefix .. current.id, cjson.encode(row))
				redis.call("HSET", KEYS[4], channel .. "|" .. model_id, current.id)
				table.insert(updated, model_id)
			else
				local id = tostring(redis.call("INCR", KEYS[2]))
				row.id = id
				redis.call("SET", row_prefix .. id, cjson.encode(row))
				redis.call("SADD", KEYS[1], id)
				redis.call("HSETNX", KEYS[3], model_id, id)
				redis.call("HSET", KEYS[4], channel .. "|" .. model_id, id)
				table.insert(added, model_id)
			end
		end
		if prune then
			for model_id, current in pairs(existing) do
				local current_provider = string.lower(tostring(current.row.provider or ""))
				if not wanted[model_id] and (provider_scope == "" or current_provider == provider_scope) then
					local origin = string.lower(tostring(current.row.origin or ""))
					if (provider_scope ~= "" and prune) or origin == "discovery" then
						redis.call("DEL", row_prefix .. current.id)
						redis.call("SREM", KEYS[1], current.id)
						if redis.call("HGET", KEYS[3], model_id) == current.id then redis.call("HDEL", KEYS[3], model_id) end
						local index_key = channel .. "|" .. model_id
						if redis.call("HGET", KEYS[4], index_key) == current.id then redis.call("HDEL", KEYS[4], index_key) end
						table.insert(deleted, model_id)
					elseif origin == "" or origin == "manual" then table.insert(protected, model_id) end
				end
			end
		end
		return cjson.encode({added = added, updated = updated, deleted = deleted, protected = protected})
	`)
)

type apiKeyRecord struct {
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

func newRedisStore(addr, password string, db int, prefix string, credentialKey []byte) (*redisStore, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, fmt.Errorf("redis address is required")
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "orchids:"
	}
	if !strings.HasSuffix(prefix, ":") {
		prefix += ":"
	}

	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		PoolSize:     200,
		MinIdleConns: 20,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}

	credentials, err := newCredentialCipher(credentialKey)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	s := &redisStore{
		client:        client,
		prefix:        prefix,
		credentials:   credentials,
		changePending: make(map[int64]AccountChange),
		changeWake:    make(chan struct{}, 1),
		changeStop:    make(chan struct{}),
		changeDone:    make(chan struct{}),
	}
	go s.dispatchChanges()
	return s, nil
}

func (s *redisStore) Client() *redis.Client {
	if s == nil {
		return nil
	}
	return s.client
}

func (s *redisStore) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	if s.changeStop != nil && s.changeDone != nil {
		s.changeOnce.Do(func() {
			s.changeMu.Lock()
			s.changeClosed = true
			s.changeMu.Unlock()
			close(s.changeStop)
		})
		// A permanently blocked external emitter must not make service shutdown hang.
		// Normal emitters drain completely; after the grace period Redis still closes
		// and the process can terminate.
		select {
		case <-s.changeDone:
		case <-time.After(2 * time.Second):
			slog.Warn("Account change dispatcher did not stop before store close")
		}
	}
	return s.client.Close()
}

func (s *redisStore) CreateAccount(ctx context.Context, acc *Account) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}

	id, err := s.client.Incr(ctx, s.accountsNextIDKey()).Result()
	if err != nil {
		return err
	}

	now := time.Now()
	acc.ID = id
	if acc.CreatedAt.IsZero() {
		acc.CreatedAt = now
	}
	if acc.UpdatedAt.IsZero() {
		acc.UpdatedAt = now
	}

	data, err := s.marshalAccount(acc)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.accountsKey(id), data, 0)
	pipe.SAdd(ctx, s.accountsIDsKey(), id)
	if acc.Enabled {
		pipe.SAdd(ctx, s.accountsEnabledKey(), id)
	} else {
		pipe.SRem(ctx, s.accountsEnabledKey(), id)
	}
	if _, err = pipe.Exec(ctx); err != nil {
		return err
	}
	// Only a write that reached Redis is announced: a subscriber must never react
	// to a change that did not happen.
	s.publishChange(ctx, nil, id)
	return nil
}

// mergeModelCooldowns combines two per-model cooldown maps, keeping the later
// deadline for each model and discarding entries that have already expired.
func mergeModelCooldowns(existing, incoming map[string]time.Time) map[string]time.Time {
	if len(existing) == 0 && len(incoming) == 0 {
		return nil
	}
	now := time.Now()
	merged := make(map[string]time.Time, len(existing)+len(incoming))
	for _, source := range []map[string]time.Time{existing, incoming} {
		for model, until := range source {
			name := strings.TrimSpace(model)
			if name == "" || until.IsZero() || !until.After(now) {
				continue
			}
			if current, ok := merged[name]; !ok || until.After(current) {
				merged[name] = until
			}
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// RecordModelCooldown marks one model of an account as throttled until the given
// deadline. Only the named model is affected: the account stays in the pool for
// its other models, which is the difference between "this model is hot" and
// "this account is dead".
func RecordModelCooldown(acc *Account, model string, until time.Time) {
	if acc == nil || until.IsZero() || !until.After(time.Now()) {
		return
	}
	name := strings.TrimSpace(model)
	if name == "" {
		return
	}
	if acc.ModelCooldowns == nil {
		acc.ModelCooldowns = map[string]time.Time{}
	}
	if current, ok := acc.ModelCooldowns[name]; !ok || until.After(current) {
		acc.ModelCooldowns[name] = until
	}
}

// ModelCooldownRemaining reports how long the account is throttled for one model,
// or zero when it may be used. It is the single reader of ModelCooldowns, so the
// pool and the request path agree on what "cooling down" means.
func ModelCooldownRemaining(acc *Account, model string, now time.Time) time.Duration {
	if acc == nil || len(acc.ModelCooldowns) == 0 {
		return 0
	}
	until, ok := acc.ModelCooldowns[strings.TrimSpace(model)]
	if !ok || until.IsZero() {
		return 0
	}
	if !until.After(now) {
		return 0
	}
	return until.Sub(now)
}

func (s *redisStore) UpdateAccount(ctx context.Context, acc *Account) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if acc.ID == 0 {
		return nil
	}

	return s.updateAccountAtomic(ctx, acc.ID, func(existing *Account) error {
		updated := *existing
		updated.Name = acc.Name
		if acc.AccountType == "" {
			updated.AccountType = existing.AccountType
		} else {
			updated.AccountType = acc.AccountType
		}
		updated.SessionID = acc.SessionID
		updated.ClientCookie = acc.ClientCookie
		updated.RefreshToken = acc.RefreshToken
		if acc.SessionCookie == "" {
			updated.SessionCookie = existing.SessionCookie
		} else {
			updated.SessionCookie = acc.SessionCookie
		}
		updated.ClientUat = acc.ClientUat
		updated.UserID = acc.UserID
		updated.AgentMode = acc.AgentMode
		updated.Email = acc.Email
		updated.Weight = acc.Weight
		updated.MaxConcurrent = acc.MaxConcurrent
		updated.Enabled = acc.Enabled
		updated.Token = acc.Token
		updated.Subscription = acc.Subscription
		updated.UsageCurrent = acc.UsageCurrent
		// UsageTotal and the daily token fields are gateway-owned atomic counters.
		// Copying them from an Account snapshot races IncrementAccountStats: a
		// status/quota update loaded before an increment would write the old values
		// back afterward and silently lose usage. Only the increment script mutates
		// these fields after account creation.
		updated.UsageLimit = acc.UsageLimit
		updated.StatusCode = acc.StatusCode
		updated.AuthStatus = acc.AuthStatus
		if acc.ClearVerifiedAt {
			updated.AuthStatus = AccountAuthStatusActive
		}
		updated.RateLimitFailures = acc.RateLimitFailures
		// The reason describes the CURRENT status only. It must never outlive the
		// status it explains, or a recovered account keeps showing a stale error.
		if strings.TrimSpace(acc.StatusCode) == "" {
			updated.StatusMessage = ""
		} else {
			updated.StatusMessage = acc.StatusMessage
		}
		updated.LastAttempt = acc.LastAttempt
		// A verdict timestamp is monotonic per credential: an unrelated partial
		// update (request counters, quota rotation) must not un-verify an account.
		// Replacing a credential clears it explicitly via ClearVerifiedAt.
		switch {
		case acc.ClearVerifiedAt:
			updated.VerifiedAt = time.Time{}
		case !acc.VerifiedAt.IsZero():
			updated.VerifiedAt = acc.VerifiedAt
		}
		updated.ClearVerifiedAt = false
		// A request may persist its verdict after a background quota refresh has
		// already written a newer reset. Never let that stale request shorten or
		// erase the authoritative deadline. A current snapshot may still clear it.
		staleSnapshot := !acc.UpdatedAt.IsZero() && acc.UpdatedAt.Before(existing.UpdatedAt)
		switch {
		case acc.QuotaResetAt.After(updated.QuotaResetAt):
			updated.QuotaResetAt = acc.QuotaResetAt
		case !staleSnapshot:
			updated.QuotaResetAt = acc.QuotaResetAt
		}
		// Grok Build CLI OAuth credentials and identity must survive refresh /
		// admin updates. Leaving these out would silently drop rotated tokens.
		if strings.TrimSpace(acc.CredentialType) == "" {
			updated.CredentialType = existing.CredentialType
		} else {
			updated.CredentialType = acc.CredentialType
		}
		updated.OAuthAccessToken = acc.OAuthAccessToken
		updated.OAuthRefreshToken = acc.OAuthRefreshToken
		updated.OAuthExpiresAt = acc.OAuthExpiresAt
		if strings.TrimSpace(acc.TeamID) == "" {
			updated.TeamID = existing.TeamID
		} else {
			updated.TeamID = acc.TeamID
		}
		if strings.TrimSpace(acc.GrokProvider) == "" {
			updated.GrokProvider = existing.GrokProvider
		} else {
			updated.GrokProvider = acc.GrokProvider
		}
		// Account updates are often partial (for example request counters and
		// credential rotation). Provider snapshots are refreshed independently, so
		// never erase a successfully observed catalog/billing window with a zero
		// value from an unrelated update.
		if acc.GrokModels != nil && (!staleSnapshot || acc.GrokModelsSyncedAt.After(existing.GrokModelsSyncedAt)) {
			updated.GrokModels = append([]string(nil), acc.GrokModels...)
		}
		if acc.GrokModelCatalog != nil && (!staleSnapshot || acc.GrokModelsSyncedAt.After(existing.GrokModelsSyncedAt)) {
			updated.GrokModelCatalog = modelcatalog.CloneProfiles(acc.GrokModelCatalog)
		}
		if !acc.GrokModelsSyncedAt.IsZero() && (!staleSnapshot || acc.GrokModelsSyncedAt.After(existing.GrokModelsSyncedAt)) {
			updated.GrokModelsSyncedAt = acc.GrokModelsSyncedAt
		}
		if !acc.GrokBilling.SyncedAt.IsZero() {
			updated.GrokBilling = acc.GrokBilling
		}
		if !acc.GrokRateLimits.ObservedAt.IsZero() {
			updated.GrokRateLimits = acc.GrokRateLimits
		}
		if !acc.GrokFreeQuota.ConfirmedAt.IsZero() {
			updated.GrokFreeQuota = acc.GrokFreeQuota
		}
		// Per-model cooldowns are merged rather than replaced: an update written by a
		// path that did not touch them (a request counter, a quota refresh) must not
		// drop a cooldown another path just recorded.
		updated.ModelCooldowns = mergeModelCooldowns(existing.ModelCooldowns, acc.ModelCooldowns)
		// WorkBuddy credentials are rotated by the upstream (Keycloak rotates the
		// refresh token on every renewal) and account updates are frequently
		// partial, so an empty value means "keep what is stored", never "erase".
		if acc.ReplaceWorkBuddyCredentials {
			updated.WorkBuddyAccessToken = strings.TrimSpace(acc.WorkBuddyAccessToken)
			updated.WorkBuddyRefreshToken = strings.TrimSpace(acc.WorkBuddyRefreshToken)
			updated.WorkBuddyExpiresAt = acc.WorkBuddyExpiresAt
		}
		if token := strings.TrimSpace(acc.WorkBuddyUID); token != "" {
			updated.WorkBuddyUID = token
		}
		if len(acc.WorkBuddyModelIDs) > 0 {
			updated.WorkBuddyModelIDs = append([]string(nil), acc.WorkBuddyModelIDs...)
		}
		if !acc.WorkBuddyModelsSyncedAt.IsZero() {
			updated.WorkBuddyModelsSyncedAt = acc.WorkBuddyModelsSyncedAt
		}
		if !acc.WorkBuddyQuota.SyncedAt.IsZero() {
			updated.WorkBuddyQuota = acc.WorkBuddyQuota
		}
		// Qoder credentials are rotated by the upstream and account updates are
		// frequently partial (a request counter, a quota refresh), so an empty value
		// means "keep what is stored", never "erase". The derived runtime pair is
		// written once at login and then reused, which is why it follows the same
		// keep-on-empty rule instead of being regenerated per request.
		if acc.ReplaceQoderCredentials {
			updated.QoderAccessToken = strings.TrimSpace(acc.QoderAccessToken)
			updated.QoderRefreshToken = strings.TrimSpace(acc.QoderRefreshToken)
			updated.QoderExpiresAt = acc.QoderExpiresAt
			updated.QoderMachineID = strings.TrimSpace(acc.QoderMachineID)
			updated.QoderRuntimeInfo = strings.TrimSpace(acc.QoderRuntimeInfo)
			updated.QoderRuntimeKey = strings.TrimSpace(acc.QoderRuntimeKey)
		}
		if token := strings.TrimSpace(acc.QoderUserID); token != "" {
			updated.QoderUserID = token
		}
		if token := strings.TrimSpace(acc.QoderUserName); token != "" {
			updated.QoderUserName = token
		}
		if token := strings.TrimSpace(acc.QoderOrganizationID); token != "" {
			updated.QoderOrganizationID = token
		}
		if len(acc.QoderOrganizationTags) > 0 {
			updated.QoderOrganizationTags = append([]string(nil), acc.QoderOrganizationTags...)
		}
		if acc.QoderDataPolicy {
			updated.QoderDataPolicy = true
		}
		if len(acc.QoderModelIDs) > 0 {
			updated.QoderModelIDs = append([]string(nil), acc.QoderModelIDs...)
		}
		if !acc.QoderModelsSyncedAt.IsZero() && (existing.QoderModelsSyncedAt.IsZero() || !acc.QoderModelsSyncedAt.Before(existing.QoderModelsSyncedAt)) {
			updated.QoderModelsSyncedAt = acc.QoderModelsSyncedAt
		}
		if !acc.QoderQuota.SyncedAt.IsZero() && (existing.QoderQuota.SyncedAt.IsZero() || !acc.QoderQuota.SyncedAt.Before(existing.QoderQuota.SyncedAt)) {
			updated.QoderQuota = acc.QoderQuota
		}
		// Cline credentials are rotated by the upstream on every refresh, and
		// account updates are frequently partial, so an empty value means "keep
		// what is stored", never "erase". Only an explicit replace intent writes
		// a new pair, so a snapshot read before a rotation cannot rewind it.
		if acc.ReplaceClineCredentials {
			updated.ClineAccessToken = strings.TrimSpace(acc.ClineAccessToken)
			updated.ClineRefreshToken = strings.TrimSpace(acc.ClineRefreshToken)
			updated.ClineExpiresAt = acc.ClineExpiresAt
		}
		if email := strings.TrimSpace(acc.ClineEmail); email != "" {
			updated.ClineEmail = email
		}
		// The tier follows the same rule as the credentials: an account update
		// is frequently partial, so an empty value means "keep what is stored".
		// That matters because "no tier recorded" and "tier is free" are
		// different states, and only one of them is evidence.
		if plan := strings.TrimSpace(acc.ClinePlan); plan != "" {
			updated.ClinePlan = plan
		}
		if len(acc.ClineModelIDs) > 0 {
			updated.ClineModelIDs = append([]string(nil), acc.ClineModelIDs...)
		}
		if !acc.ClineModelsSyncedAt.IsZero() && (existing.ClineModelsSyncedAt.IsZero() || !acc.ClineModelsSyncedAt.Before(existing.ClineModelsSyncedAt)) {
			updated.ClineModelsSyncedAt = acc.ClineModelsSyncedAt
		}
		*existing = updated
		return nil
	})
}

var errAccountUnchanged = fmt.Errorf("account unchanged")

// updateAccountAtomic applies a field mutation with optimistic locking. Every
// account writer uses the same watched key, so a quota/stat update that lands
// between read and write causes a retry instead of being silently overwritten.
// UpdateAccountQuality persists the quality guard's verdict.
//
// UpdateAccount copies a fixed field list for safety and does not include these
// two, so a park decided by the guard was only ever applied to the in-memory
// account: the next load from Redis brought the credential straight back into
// rotation. This writes exactly the verdict and nothing else.
func (s *redisStore) UpdateAccountQuality(ctx context.Context, id int64, failures int, cooldownUntil time.Time) error {
	if failures < 0 {
		failures = 0
	}
	return s.updateAccountAtomic(ctx, id, func(existing *Account) error {
		existing.QualityFailures = failures
		existing.QualityCooldownUntil = cooldownUntil
		return nil
	})
}

func (s *redisStore) updateAccountAtomic(ctx context.Context, id int64, mutate func(*Account) error) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return nil
	}
	key := s.accountsKey(id)
	for attempt := 0; attempt < 8; attempt++ {
		var previous *Account
		err := s.client.Watch(ctx, func(tx *redis.Tx) error {
			value, err := tx.Get(ctx, key).Bytes()
			if err == redis.Nil {
				return ErrNoRows
			}
			if err != nil {
				return err
			}
			legacyCredential, err := hasLegacyCredential(value)
			if err != nil {
				return err
			}
			current, err := s.unmarshalAccount(value, id)
			if err != nil {
				return err
			}
			copied := *current
			previous = &copied
			if err := mutate(current); err != nil {
				return err
			}
			// UpdatedAt describes a persisted semantic change. Do not rewrite the
			// row or publish an event when a refresh observed exactly the state we
			// already have.
			current.UpdatedAt = previous.UpdatedAt
			// A semantic no-op must still rewrite legacy plaintext credentials so
			// the normal encrypted marshal path can complete the migration.
			if reflect.DeepEqual(current, previous) && !(legacyCredential && s.credentials != nil) {
				return errAccountUnchanged
			}
			current.UpdatedAt = time.Now()
			if !current.UpdatedAt.After(previous.UpdatedAt) {
				// Some platforms expose a coarser wall-clock resolution than the
				// update rate. UpdatedAt is also the stale-snapshot version marker,
				// so equal timestamps must still advance monotonically.
				current.UpdatedAt = previous.UpdatedAt.Add(time.Nanosecond)
			}
			data, err := s.marshalAccount(current)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, data, 0)
				pipe.SAdd(ctx, s.accountsIDsKey(), id)
				if current.Enabled {
					pipe.SAdd(ctx, s.accountsEnabledKey(), id)
				} else {
					pipe.SRem(ctx, s.accountsEnabledKey(), id)
				}
				return nil
			})
			return err
		}, key)
		if err == redis.TxFailedErr {
			continue
		}
		if err == errAccountUnchanged {
			return nil
		}
		if err == ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		s.publishChange(ctx, previous, id)
		return nil
	}
	return fmt.Errorf("account %d changed too frequently; update could not be committed", id)
}

func (s *redisStore) UpdateWorkBuddyCredentials(ctx context.Context, id int64, patch WorkBuddyCredentialPatch) error {
	return s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		if expected := strings.TrimSpace(patch.ExpectedRefreshToken); expected != "" &&
			acc.WorkBuddyRefreshToken != expected && acc.WorkBuddyRefreshToken != strings.TrimSpace(patch.RefreshToken) {
			return fmt.Errorf("workbuddy credential changed concurrently")
		}
		if token := strings.TrimSpace(patch.AccessToken); token != "" {
			acc.WorkBuddyAccessToken = token
		}
		if token := strings.TrimSpace(patch.RefreshToken); token != "" {
			acc.WorkBuddyRefreshToken = token
			acc.ClientCookie = token
		}
		if !patch.ExpiresAt.IsZero() {
			acc.WorkBuddyExpiresAt = patch.ExpiresAt
		}
		if uid := strings.TrimSpace(patch.UID); uid != "" {
			acc.WorkBuddyUID = uid
		}
		if email := strings.TrimSpace(patch.Email); email != "" && strings.TrimSpace(acc.Email) == "" {
			acc.Email = email
		}
		return nil
	})
}

func (s *redisStore) UpdateQoderAccount(ctx context.Context, id int64, patch QoderAccountPatch) error {
	return s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		if expected := strings.TrimSpace(patch.ExpectedRefreshToken); expected != "" &&
			acc.QoderRefreshToken != expected && acc.QoderRefreshToken != strings.TrimSpace(patch.RefreshToken) {
			return fmt.Errorf("qoder credential changed concurrently")
		}
		if token := strings.TrimSpace(patch.AccessToken); token != "" {
			acc.QoderAccessToken = token
		}
		if token := strings.TrimSpace(patch.RefreshToken); token != "" {
			acc.QoderRefreshToken = token
		}
		if !patch.ExpiresAt.IsZero() {
			acc.QoderExpiresAt = patch.ExpiresAt
		}
		if uid := strings.TrimSpace(patch.UserID); uid != "" {
			acc.QoderUserID = uid
		}
		if value := strings.TrimSpace(patch.RuntimeInfo); value != "" {
			acc.QoderRuntimeInfo = value
		}
		if value := strings.TrimSpace(patch.RuntimeKey); value != "" {
			acc.QoderRuntimeKey = value
		}
		if patch.ModelIDs != nil {
			acc.QoderModelIDs = append([]string(nil), patch.ModelIDs...)
		}
		return nil
	})
}

func (s *redisStore) UpdateClineCredentials(ctx context.Context, id int64, patch ClineCredentialPatch) error {
	return s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		if expected := strings.TrimSpace(patch.ExpectedRefreshToken); expected != "" &&
			acc.ClineRefreshToken != expected && acc.ClineRefreshToken != strings.TrimSpace(patch.RefreshToken) {
			return fmt.Errorf("cline credential changed concurrently")
		}
		if token := strings.TrimSpace(patch.AccessToken); token != "" {
			acc.ClineAccessToken = token
		}
		if token := strings.TrimSpace(patch.RefreshToken); token != "" {
			acc.ClineRefreshToken = token
		}
		if !patch.ExpiresAt.IsZero() {
			acc.ClineExpiresAt = patch.ExpiresAt
		}
		if email := strings.TrimSpace(patch.Email); email != "" {
			acc.ClineEmail = email
		}
		if patch.ModelIDs != nil {
			acc.ClineModelIDs = append([]string(nil), patch.ModelIDs...)
		}
		return nil
	})
}

func (s *redisStore) DeleteAccount(ctx context.Context, id int64) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return nil
	}

	// Read the row before removing it so the notification can say what was
	// removed, and so a delete of a non-existent id stays silent.
	previous, previousErr := s.getAccount(ctx, id)
	if previousErr != nil && previousErr != ErrNoRows {
		return previousErr
	}

	pipe := s.client.Pipeline()
	pipe.Del(ctx, s.accountsKey(id), s.accountStatsOperationsKey(id))
	pipe.SRem(ctx, s.accountsIDsKey(), id)
	pipe.SRem(ctx, s.accountsEnabledKey(), id)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	if previousErr == nil {
		s.publishChange(ctx, previous, id)
	}
	return nil
}

func (s *redisStore) GetAccount(ctx context.Context, id int64) (*Account, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	return s.getAccount(ctx, id)
}

func (s *redisStore) ListAccounts(ctx context.Context) ([]*Account, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	ids, err := s.client.SMembers(ctx, s.accountsIDsKey()).Result()
	if err != nil {
		return nil, err
	}
	return s.getAccountsByIDs(ctx, ids, false)
}

func (s *redisStore) GetEnabledAccounts(ctx context.Context) ([]*Account, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	ids, err := s.client.SMembers(ctx, s.accountsEnabledKey()).Result()
	if err != nil {
		return nil, err
	}
	return s.getAccountsByIDs(ctx, ids, true)
}

func (s *redisStore) IncrementAccountStats(ctx context.Context, id int64, usage float64, count int64) error {
	return s.IncrementAccountStatsOperation(ctx, id, usage, count, "", time.Now().UTC())
}

func (s *redisStore) IncrementAccountStatsOperation(ctx context.Context, id int64, usage float64, count int64, operationID string, completedAt time.Time) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return nil
	}
	if usage <= 0 && count <= 0 {
		return nil
	}
	now := time.Now().UTC()
	if completedAt.IsZero() {
		completedAt = now
	} else {
		completedAt = completedAt.UTC()
	}
	nowStr := now.Format(time.RFC3339Nano)
	// Daily counters belong to request completion, not a delayed retry. This
	// avoids moving a pre-midnight completion into the following UTC day.
	today := completedAt.Format("2006-01-02")
	keys := []string{s.accountsKey(id), s.accountStatsOperationsKey(id)}
	args := []interface{}{usage, count, nowStr, today, strings.TrimSpace(operationID)}

	err := incrementAccountStatsScript.Run(ctx, s.client, keys, args...).Err()
	if err != nil && err != redis.Nil {
		return err
	}
	return nil
}

func decrementQuotaWindow(window *GrokQuotaWindow, amount float64) bool {
	if window == nil || !window.HasRemaining || window.Remaining <= 0 || amount <= 0 {
		return false
	}
	window.Remaining = max(0, window.Remaining-amount)
	return true
}

func (s *redisStore) ConsumeGrokQuota(ctx context.Context, id int64, provider string, amount float64) (bool, error) {
	if amount <= 0 || id == 0 {
		return false, nil
	}
	consumed := false
	err := s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		consumed = false // updateAccountAtomic may retry after a WATCH conflict.
		now := time.Now().UTC()
		if !strings.EqualFold(strings.TrimSpace(provider), "build") {
			return nil
		}
		consumed = decrementQuotaWindow(&acc.GrokRateLimits.Requests, amount)
		if consumed {
			acc.GrokRateLimits.ObservedAt = now
		}
		return nil
	})
	return consumed, err
}

func (s *redisStore) ClaimGrokPaidQuotaProbe(ctx context.Context, id int64, now time.Time) (bool, error) {
	if id == 0 {
		return false, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	claimed := false
	err := s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		claimed = false // updateAccountAtomic may retry after a WATCH conflict.
		billing := &acc.GrokBilling
		if !billing.IsExhausted() {
			return nil
		}
		due := billing.NextProbeAt
		if due.IsZero() {
			due = billing.PeriodEnd()
		}
		if due.IsZero() || now.Before(due) {
			return nil
		}
		billing.LastProbeAt = now
		billing.NextProbeAt = now.Add(GrokPaidQuotaProbeInterval)
		claimed = true
		return nil
	})
	return claimed, err
}

func (s *redisStore) getAccount(ctx context.Context, id int64) (*Account, error) {
	if id == 0 {
		return nil, ErrNoRows
	}
	value, err := s.client.Get(ctx, s.accountsKey(id)).Result()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}

	return s.unmarshalAccount([]byte(value), id)
}

// getAccountsByIDsPipelined 使用 Pipeline 批量获取账号数据
func (s *redisStore) getAccountsByIDsPipelined(ctx context.Context, keys []string) ([]interface{}, error) {
	if len(keys) == 0 {
		return nil, nil
	}

	pipe := s.client.Pipeline()
	cmds := make([]*redis.StringCmd, len(keys))

	// 批量添加 GET 命令到 Pipeline
	for i, key := range keys {
		cmds[i] = pipe.Get(ctx, key)
	}

	// 执行 Pipeline
	_, err := pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return nil, err
	}

	// 收集结果
	values := make([]interface{}, len(cmds))
	for i, cmd := range cmds {
		val, err := cmd.Result()
		if err == redis.Nil {
			values[i] = nil
		} else if err != nil {
			// 部分命令失败，返回错误触发回退
			return nil, err
		} else {
			values[i] = val
		}
	}

	return values, nil
}

func (s *redisStore) getAccountsByIDs(ctx context.Context, ids []string, onlyEnabled bool) ([]*Account, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	idNums := parseSortedInt64s(ids)
	if len(idNums) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(idNums))
	for _, id := range idNums {
		keys = append(keys, s.accountsKey(id))
	}

	// 尝试使用 Pipeline 批量获取
	values, err := s.getAccountsByIDsPipelined(ctx, keys)
	if err != nil {
		// Pipeline 失败，回退到单命令模式
		values, err = s.client.MGet(ctx, keys...).Result()
		if err != nil {
			return nil, err
		}
	}

	results := make([]*Account, len(values))
	decodeErrs := make([]error, len(values))
	decode := func(i int) {
		strVal, ok := values[i].(string)
		if !ok || strVal == "" {
			return
		}
		acc, err := s.unmarshalAccount([]byte(strVal), idNums[i])
		if err != nil {
			decodeErrs[i] = err
			return
		}
		if onlyEnabled && !acc.Enabled {
			return
		}
		results[i] = acc
	}
	if len(values) >= redisBatchParallelThreshold {
		util.ParallelFor(len(values), decode)
	} else {
		for i := range values {
			decode(i)
		}
	}
	for _, decodeErr := range decodeErrs {
		if decodeErr != nil {
			return nil, decodeErr
		}
	}

	accounts := make([]*Account, 0, len(values))
	for _, acc := range results {
		if acc != nil {
			accounts = append(accounts, acc)
		}
	}
	return accounts, nil
}

func (s *redisStore) GetSetting(ctx context.Context, key string) (string, error) {
	if s == nil || s.client == nil {
		return "", fmt.Errorf("redis store not configured")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", nil
	}
	value, err := s.client.Get(ctx, s.settingsKey(key)).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

func (s *redisStore) SetSetting(ctx context.Context, key, value string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	return s.client.Set(ctx, s.settingsKey(key), value, 0).Err()
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
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
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
	existing, err := s.getApiKeyByID(ctx, key.ID)
	if err != nil {
		return err
	}
	record := apiKeyRecordFromKey(key)
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.apiKeysKey(key.ID), data, 0)
	if existing.KeyHash != record.KeyHash {
		if existing.KeyHash != "" {
			pipe.Del(ctx, s.apiKeysHashKey(existing.KeyHash))
		}
		if record.KeyHash != "" {
			pipe.Set(ctx, s.apiKeysHashKey(record.KeyHash), key.ID, 0)
		}
	}
	// Keep the reservation mirror in step with the stored policy. The settled
	// usage counter is ledger state, not a policy field, so an update never
	// rewrites it.
	pipe.Set(ctx, s.apiKeyBillingLimitKey(key.ID), record.BillingLimitUSDTicks, 0)
	_, err = pipe.Exec(ctx)
	return err
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
	id, err := s.client.Get(ctx, s.apiKeysHashKey(hash)).Int64()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	return s.getApiKeyByID(ctx, id)
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
	if len(values) >= redisBatchParallelThreshold {
		util.ParallelFor(len(values), decode)
	} else {
		for i := range values {
			decode(i)
		}
	}

	items := make([]*ApiKey, 0, len(values))
	for _, key := range results {
		if key != nil {
			items = append(items, key)
		}
	}
	return items, nil
}

func parseSortedInt64s(values []string) []int64 {
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		if id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (s *redisStore) accountsKey(id int64) string {
	return fmt.Sprintf("%saccounts:id:%d", s.prefix, id)
}

func (s *redisStore) accountStatsOperationsKey(id int64) string {
	return fmt.Sprintf("%saccounts:stats_ops:%d", s.prefix, id)
}

func (s *redisStore) accountsIDsKey() string {
	return s.prefix + "accounts:ids"
}

func (s *redisStore) accountsEnabledKey() string {
	return s.prefix + "accounts:enabled"
}

func (s *redisStore) accountsNextIDKey() string {
	return s.prefix + "accounts:next_id"
}

func (s *redisStore) settingsKey(key string) string {
	return s.prefix + "settings:" + key
}

func (s *redisStore) apiKeysKey(id int64) string {
	return fmt.Sprintf("%sapi_keys:id:%d", s.prefix, id)
}

func (s *redisStore) apiKeysIDsKey() string {
	return s.prefix + "api_keys:ids"
}

func (s *redisStore) apiKeysNextIDKey() string {
	return s.prefix + "api_keys:next_id"
}

func (s *redisStore) apiKeysHashKey(hash string) string {
	return s.prefix + "api_keys:hash:" + hash
}

func (s *redisStore) apiKeyRPMKey(id, minute int64) string {
	return fmt.Sprintf("%sapi_keys:rpm:%d:%d", s.prefix, id, minute)
}

// apiKeyBillingReservationsKey holds the live holds of one key as a hash of
// "eventID -> <amount ticks>:<expiry unix seconds>". Expired fields are pruned
// by the reserve script, and the hash itself expires shortly after its last hold.
func (s *redisStore) apiKeyBillingReservationsKey(id int64) string {
	return fmt.Sprintf("%skeybilling:res:%d", s.prefix, id)
}

// apiKeyBillingUsedKey is the settled usage counter in ticks. It is the source
// of truth for how much a key has spent.
func (s *redisStore) apiKeyBillingUsedKey(id int64) string {
	return fmt.Sprintf("%skeybilling:used:%d", s.prefix, id)
}

func (s *redisStore) apiKeyBillingSettledKey(id int64) string {
	return fmt.Sprintf("%skeybilling:settled:%d", s.prefix, id)
}

// apiKeyBillingLimitKey mirrors the key's billing limit so the reserve script
// can decide without loading and parsing the key record.
func (s *redisStore) apiKeyBillingLimitKey(id int64) string {
	return fmt.Sprintf("%skeybilling:limit:%d", s.prefix, id)
}

func (s *redisStore) storedResponseKey(responseID, ownerHash string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(ownerHash) + "\x00" + strings.TrimSpace(responseID)))
	return s.prefix + "responses:ownership:" + hex.EncodeToString(digest[:])
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
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(model)) + "\x00" + strings.TrimSpace(sessionKey)))
	return s.prefix + "grok:reasoning_replay:" + hex.EncodeToString(digest[:])
}

func (s *redisStore) DeleteReasoningReplay(ctx context.Context, model, key string) error {
	return s.client.Del(ctx, s.reasoningReplayKey(model, key)).Err()
}

func (s *redisStore) SaveReasoningReplay(ctx context.Context, replay *StoredReasoningReplay, ttl time.Duration) error {
	if replay == nil || strings.TrimSpace(replay.Model) == "" || strings.TrimSpace(replay.SessionKey) == "" || strings.TrimSpace(replay.EncryptedContent) == "" {
		return fmt.Errorf("invalid reasoning replay")
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
	raw, err := s.client.Get(ctx, s.reasoningReplayKey(model, sessionKey)).Bytes()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	var replay StoredReasoningReplay
	if err := json.Unmarshal(raw, &replay); err != nil {
		return nil, err
	}
	if !replay.ExpiresAt.IsZero() && !time.Now().UTC().Before(replay.ExpiresAt) {
		_ = s.client.Del(ctx, s.reasoningReplayKey(model, sessionKey)).Err()
		return nil, ErrNoRows
	}
	return &replay, nil
}

func (s *redisStore) sessionAffinityKey(provider, model, sessionKey string) string {
	source := strings.ToLower(strings.TrimSpace(provider)) + "\x00" + strings.ToLower(strings.TrimSpace(model)) + "\x00" + strings.TrimSpace(sessionKey)
	digest := sha256.Sum256([]byte(source))
	return s.prefix + "grok:session_affinity:" + hex.EncodeToString(digest[:])
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
	raw, err := s.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	var affinity StoredSessionAffinity
	if err := json.Unmarshal(raw, &affinity); err != nil {
		return nil, err
	}
	if !affinity.ExpiresAt.IsZero() && !time.Now().UTC().Before(affinity.ExpiresAt) {
		_ = s.client.Del(ctx, key).Err()
		return nil, ErrNoRows
	}
	return &affinity, nil
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

// Model wrappers

func (s *redisStore) CreateModel(ctx context.Context, m *Model) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}

	// Use a counter for ID generation to match screenshot style (numeric)
	id, err := s.client.Incr(ctx, s.modelsNextIDKey()).Result()
	if err != nil {
		return err
	}
	m.ID = strconv.FormatInt(id, 10)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}

	data, err := json.Marshal(m)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.modelsKey(m.ID), data, 0)
	pipe.SAdd(ctx, s.modelsIDsKey(), m.ID)
	if strings.TrimSpace(m.ModelID) != "" {
		pipe.HSetNX(ctx, s.modelsModelIDMapKey(), m.ModelID, m.ID)
		pipe.HSet(ctx, s.modelsChannelModelIDMapKey(), modelChannelIndexKey(m.Channel, m.ModelID), m.ID)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) UpdateModel(ctx context.Context, m *Model) error {
	if m != nil && m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if m.ID == "" {
		return fmt.Errorf("model id is required")
	}

	prev, _ := s.GetModel(ctx, m.ID)

	data, err := json.Marshal(m)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.modelsKey(m.ID), data, 0)
	pipe.SAdd(ctx, s.modelsIDsKey(), m.ID)
	if prev != nil && strings.TrimSpace(prev.ModelID) != "" {
		prevKey := modelChannelIndexKey(prev.Channel, prev.ModelID)
		nextKey := modelChannelIndexKey(m.Channel, m.ModelID)
		if prevKey != nextKey {
			pipe.HDel(ctx, s.modelsChannelModelIDMapKey(), prevKey)
		}
	}
	if strings.TrimSpace(m.ModelID) != "" {
		currentGlobalID, _ := s.client.HGet(ctx, s.modelsModelIDMapKey(), m.ModelID).Result()
		if currentGlobalID == "" || currentGlobalID == m.ID || (prev != nil && currentGlobalID == prev.ID) {
			pipe.HSet(ctx, s.modelsModelIDMapKey(), m.ModelID, m.ID)
		}
		pipe.HSet(ctx, s.modelsChannelModelIDMapKey(), modelChannelIndexKey(m.Channel, m.ModelID), m.ID)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) DeleteModel(ctx context.Context, id string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == "" {
		return nil
	}

	// Fetch model to get ModelID for index cleanup
	m, _ := s.GetModel(ctx, id)

	pipe := s.client.Pipeline()
	pipe.Del(ctx, s.modelsKey(id))
	pipe.SRem(ctx, s.modelsIDsKey(), id)
	if m != nil && strings.TrimSpace(m.ModelID) != "" {
		currentGlobalID, _ := s.client.HGet(ctx, s.modelsModelIDMapKey(), m.ModelID).Result()
		if currentGlobalID == id {
			pipe.HDel(ctx, s.modelsModelIDMapKey(), m.ModelID)
		}
		pipe.HDel(ctx, s.modelsChannelModelIDMapKey(), modelChannelIndexKey(m.Channel, m.ModelID))
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *redisStore) GetModel(ctx context.Context, id string) (*Model, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	value, err := s.client.Get(ctx, s.modelsKey(id)).Result()
	if err == redis.Nil {
		return nil, ErrNoRows // reuse ErrNoRows for consistency
	}
	if err != nil {
		return nil, err
	}

	var m Model
	if err := json.Unmarshal([]byte(value), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *redisStore) ListModels(ctx context.Context) ([]*Model, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	values, err := listModelsScript.Run(ctx, s.client, []string{s.modelsIDsKey()}, s.prefix+"models:id:").StringSlice()
	if err != nil {
		return nil, err
	}

	models := make([]*Model, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		var m Model
		if err := json.Unmarshal([]byte(value), &m); err != nil {
			continue
		}
		models = append(models, &m)
	}
	sort.Slice(models, func(i, j int) bool {
		id1, err1 := strconv.Atoi(models[i].ID)
		id2, err2 := strconv.Atoi(models[j].ID)
		if err1 == nil && err2 == nil {
			return id1 < id2
		}
		return models[i].ID < models[j].ID
	})
	return models, nil
}

func (s *redisStore) ReconcileDiscoveredModels(ctx context.Context, channel string, models []*Model, options ModelReconcileOptions) (*ModelReconcileResult, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	channelKey := normalizeModelChannelKey(channel)
	if channelKey == "" {
		return nil, fmt.Errorf("model channel is required")
	}
	now := time.Now().UTC()
	rows := make([]*Model, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, input := range models {
		if input == nil {
			return nil, fmt.Errorf("discovered model is nil")
		}
		modelID := strings.TrimSpace(input.ModelID)
		if modelID == "" {
			return nil, fmt.Errorf("discovered model id is required")
		}
		if _, exists := seen[modelID]; exists {
			return nil, fmt.Errorf("duplicate discovered model id %q", modelID)
		}
		seen[modelID] = struct{}{}
		row := *input
		row.ID = ""
		row.Channel = strings.TrimSpace(channel)
		row.ModelID = modelID
		row.Origin = "discovery"
		if row.CreatedAt.IsZero() {
			row.CreatedAt = now
		}
		row.NormalizeRoute()
		rows = append(rows, &row)
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	prune := "0"
	if options.Prune {
		prune = "1"
	}
	text, err := reconcileDiscoveredModelsScript.Run(ctx, s.client, []string{
		s.modelsIDsKey(), s.modelsNextIDKey(), s.modelsModelIDMapKey(), s.modelsChannelModelIDMapKey(),
	}, s.prefix+"models:id:", channelKey, prune, payload, strings.ToLower(strings.TrimSpace(options.ProviderScope))).Text()
	if err != nil {
		return nil, err
	}
	var applied struct {
		Added     []string `json:"added"`
		Updated   []string `json:"updated"`
		Deleted   []string `json:"deleted"`
		Protected []string `json:"protected"`
	}
	if err := json.Unmarshal([]byte(text), &applied); err != nil {
		// Redis Lua represents an empty array as an empty object. Decode through
		// RawMessage so production Redis and miniredis have one stable contract.
		var raw map[string]json.RawMessage
		if rawErr := json.Unmarshal([]byte(text), &raw); rawErr != nil {
			return nil, fmt.Errorf("decode model reconciliation result: %w", err)
		}
		decodeIDs := func(key string) []string {
			value := raw[key]
			if len(value) == 0 || string(value) == "{}" || string(value) == "null" {
				return nil
			}
			var ids []string
			_ = json.Unmarshal(value, &ids)
			return ids
		}
		applied.Added, applied.Updated = decodeIDs("added"), decodeIDs("updated")
		applied.Deleted, applied.Protected = decodeIDs("deleted"), decodeIDs("protected")
	}
	for _, ids := range [][]string{applied.Added, applied.Updated, applied.Deleted, applied.Protected} {
		sort.Strings(ids)
	}
	return &ModelReconcileResult{
		Added: len(applied.Added), Updated: len(applied.Updated), Deleted: len(applied.Deleted), Protected: len(applied.Protected),
		AddedModelIDs: applied.Added, UpdatedModelIDs: applied.Updated, DeletedModelIDs: applied.Deleted, ProtectedIDs: applied.Protected,
	}, nil
}

// Helpers

func (s *redisStore) modelsKey(id string) string {
	return s.prefix + "models:id:" + id
}

func (s *redisStore) modelsIDsKey() string {
	return s.prefix + "models:ids"
}

func (s *redisStore) modelsNextIDKey() string {
	return s.prefix + "models:next_id"
}

func (s *redisStore) modelsModelIDMapKey() string {
	return s.prefix + "models:model_id_map"
}

func (s *redisStore) modelsChannelModelIDMapKey() string {
	return s.prefix + "models:channel_model_id_map"
}

func normalizeModelChannelKey(channel string) string {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return ""
	}
	channel = strings.ReplaceAll(channel, "_", "-")
	channel = strings.ReplaceAll(channel, " ", "-")
	return channel
}

func modelChannelIndexKey(channel, modelID string) string {
	return normalizeModelChannelKey(channel) + "|" + strings.TrimSpace(modelID)
}

func (s *redisStore) GetModelByModelID(ctx context.Context, modelID string) (*Model, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, fmt.Errorf("model not found")
	}

	// Try hash index first for O(1) lookup
	id, err := s.client.HGet(ctx, s.modelsModelIDMapKey(), modelID).Result()
	if err == nil && id != "" {
		m, err := s.GetModel(ctx, id)
		if err == nil && m != nil && strings.TrimSpace(m.ModelID) == modelID {
			return m, nil
		}
		// Index stale or points to a different model, fall through to scan.
	}

	// Fallback to scan (for backward compatibility with existing data)
	models, err := s.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		if m.ModelID == modelID {
			// Repair the index
			s.client.HSet(ctx, s.modelsModelIDMapKey(), modelID, m.ID)
			return m, nil
		}
	}
	return nil, fmt.Errorf("model not found")
}

func (s *redisStore) GetModelByChannelAndModelID(ctx context.Context, channel, modelID string) (*Model, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, fmt.Errorf("model not found")
	}

	channelKey := modelChannelIndexKey(channel, modelID)
	id, err := s.client.HGet(ctx, s.modelsChannelModelIDMapKey(), channelKey).Result()
	if err == nil && id != "" {
		m, err := s.GetModel(ctx, id)
		if err == nil && m != nil && strings.TrimSpace(m.ModelID) == modelID && normalizeModelChannelKey(m.Channel) == normalizeModelChannelKey(channel) {
			return m, nil
		}
		// Index stale or points to a different channel/model, fall through to scan.
	}

	models, err := s.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	wantChannel := normalizeModelChannelKey(channel)
	for _, m := range models {
		if normalizeModelChannelKey(m.Channel) == wantChannel && m.ModelID == modelID {
			s.client.HSet(ctx, s.modelsChannelModelIDMapKey(), channelKey, m.ID)
			return m, nil
		}
	}
	return nil, fmt.Errorf("model not found")
}
