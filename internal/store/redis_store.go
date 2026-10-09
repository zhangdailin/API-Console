// Package store owns every durable object the gateway keeps: accounts and their
// credentials, API keys, the model catalog, settings and the short-lived stores
// a request needs (response bodies, reasoning replay, session affinity).
//
// Redis is the only real backend; the scripts it runs are the unit of
// atomicity. Files are split by the object they persist:
//
//	store.go               the Store facade, the Account type, settings
//	store_api_keys.go      the facade's API key surface: auth, RPM, billing
//	store_models.go        the facade's model catalog surface
//	store_response.go      the facade's short-lived stores
//	quota_snapshots.go     one channel's quota/billing observation
//	account_merge.go       how a partial Account update is merged
//	account_cooldown.go    per-model cooldown deadlines and their labels
//	account_changes.go     the account-change notification fan-out
//	credential_patch.go    per-channel credential replacement
//	credential_cipher.go   credential encryption at rest
//	model.go               model catalog helpers
//	memory_response_store.go  in-memory fallback for the ephemeral stores
//	redis_store.go         the redisStore: connection, accounts, settings
//	redis_scripts.go       the Lua scripts every atomic mutation runs
//	redis_api_keys.go      API key rows, RPM and billing ledgers
//	redis_models.go        model catalog rows and their indexes
//	redis_ephemeral.go     TTL-scoped responses, replay and session affinity
//	redis_batch.go         the shared decode fan-out helpers
//
// Credentials are encrypted before they reach Redis and never appear in a log
// line or an error returned to a client; see credential_cipher.go and
// internal/errors.
package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/util"

	"github.com/redis/go-redis/v9"
)

type redisStore struct {
	client      *redis.Client
	prefix      string
	credentials *credentialCipher
	keySecrets  *credentialCipher
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
	keyIndexMu    sync.RWMutex
	keyIndex      map[string]int64
}

// newRedisStore dials Redis and starts the change dispatcher.
func newRedisStore(addr, password string, db int, prefix string, credentialKey []byte, poolSizes ...int) (*redisStore, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, fmt.Errorf("redis address is required")
	}
	prefix = strings.TrimSpace(prefix)
	prefix = util.FirstNonEmptyUntrimmed(prefix, "orchids:")
	if !strings.HasSuffix(prefix, ":") {
		prefix += ":"
	}

	poolSize := 200
	if len(poolSizes) > 0 && poolSizes[0] > 0 {
		poolSize = min(poolSizes[0], 4096)
	}
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		PoolSize:     poolSize,
		MinIdleConns: min(20, poolSize),
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
	var keySecrets *credentialCipher
	if len(credentialKey) > 0 {
		digest := sha256.Sum256(append([]byte("orchids:api-key-secret:v1:"), credentialKey...))
		keySecrets, err = newCredentialCipher(digest[:])
		if err != nil {
			_ = client.Close()
			return nil, err
		}
	}
	s := &redisStore{
		keySecrets:    keySecrets,
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
	pipeAccountMembership(ctx, pipe, s, acc.ID, acc.Enabled)
	if _, err = pipe.Exec(ctx); err != nil {
		return err
	}
	// Only a write that reached Redis is announced: a subscriber must never react
	// to a change that did not happen.
	s.publishChange(ctx, nil, id)
	return nil
}

// pipeAccountMembership keeps the id and enabled index sets in step with the
// account row written by the same pipeline.
func pipeAccountMembership(ctx context.Context, pipe redis.Pipeliner, s *redisStore, id int64, enabled bool) {
	pipe.SAdd(ctx, s.accountsIDsKey(), id)
	if enabled {
		pipe.SAdd(ctx, s.accountsEnabledKey(), id)
		return
	}
	pipe.SRem(ctx, s.accountsEnabledKey(), id)
}

// workBuddyMeterReadingIsNewer reports whether an incoming WorkBuddy account
// carries a credit-meter reading at least as new as the stored one. Only such a
// write may move the meter snapshot or the generic usage slots that mirror it:
// every other update is partial, and a partial update must not erase a number it
// never read.
func workBuddyMeterReadingIsNewer(acc, existing *Account) bool {
	if acc == nil || acc.WorkBuddyQuota.SyncedAt.IsZero() {
		return false
	}
	if existing == nil {
		return true
	}
	return !acc.WorkBuddyQuota.SyncedAt.Before(existing.WorkBuddyQuota.SyncedAt)
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
		p := accountPatch{updated: &updated, acc: acc, existing: existing}
		// A request may persist its verdict after a background quota refresh has
		// already written a newer reset. Never let that stale request shorten or
		// erase the authoritative deadline. A current snapshot may still clear it.
		p.staleSnapshot = !acc.UpdatedAt.IsZero() && acc.UpdatedAt.Before(existing.UpdatedAt)
		p.mergeIdentity()
		p.mergeUsageMeter()
		// UsageTotal and the daily token fields are gateway-owned atomic counters.
		// Copying them from an Account snapshot races IncrementAccountStats: a
		// status/quota update loaded before an increment would write the old values
		// back afterward and silently lose usage. Only the increment script mutates
		// these fields after account creation, which is why no merge step below
		// touches them.
		p.mergeStatusAndVerdict()
		p.mergeGrokCredentials()
		p.mergeModelCooldownState()
		p.mergeWorkBuddyCredentials()
		p.mergeQoderCredentials()
		p.mergeClineCredentials()
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
			if reflect.DeepEqual(current, previous) {
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
				pipeAccountMembership(ctx, pipe, s, id, current.Enabled)
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
		if err := applyCredentialPatch("workbuddy", patch.ExpectedRefreshToken, patch.AccessToken, patch.RefreshToken, patch.ExpiresAt,
			&acc.WorkBuddyAccessToken, &acc.WorkBuddyRefreshToken, &acc.WorkBuddyExpiresAt); err != nil {
			return err
		}
		patchString(&acc.WorkBuddyUID, patch.UID)
		if email := strings.TrimSpace(patch.Email); email != "" && strings.TrimSpace(acc.Email) == "" {
			acc.Email = email
		}
		return nil
	})
}

func (s *redisStore) UpdateQoderAccount(ctx context.Context, id int64, patch QoderAccountPatch) error {
	return s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		if err := applyCredentialPatch("qoder", patch.ExpectedRefreshToken, patch.AccessToken, patch.RefreshToken, patch.ExpiresAt,
			&acc.QoderAccessToken, &acc.QoderRefreshToken, &acc.QoderExpiresAt); err != nil {
			return err
		}
		patchString(&acc.QoderUserID, patch.UserID)
		patchString(&acc.QoderRuntimeInfo, patch.RuntimeInfo)
		patchString(&acc.QoderRuntimeKey, patch.RuntimeKey)
		if patch.ModelIDs != nil {
			acc.QoderModelIDs = append([]string(nil), patch.ModelIDs...)
		}
		if patch.Quota != nil {
			// Same rule as a synced snapshot: a reading at least as new as the
			// stored one wins; an older one is discarded rather than rewinding a
			// view an in-flight sync just established.
			incoming := *patch.Quota
			if acc.QoderQuota.SyncedAt.IsZero() || !incoming.SyncedAt.Before(acc.QoderQuota.SyncedAt) {
				acc.QoderQuota = incoming
			}
		}
		return nil
	})
}

func (s *redisStore) UpdateClineCredentials(ctx context.Context, id int64, patch ClineCredentialPatch) error {
	return s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		if err := applyCredentialPatch("cline", patch.ExpectedRefreshToken, patch.AccessToken, patch.RefreshToken, patch.ExpiresAt,
			&acc.ClineAccessToken, &acc.ClineRefreshToken, &acc.ClineExpiresAt); err != nil {
			return err
		}
		patchString(&acc.ClineEmail, patch.Email)
		if patch.ModelIDs != nil {
			acc.ClineModelIDs = append([]string(nil), patch.ModelIDs...)
		}
		return nil
	})
}

func (s *redisStore) UpdateGrokCredentials(ctx context.Context, id int64, patch GrokCredentialPatch) error {
	return s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		if err := applyCredentialPatch("grok", patch.ExpectedRefreshToken, patch.AccessToken, patch.RefreshToken, patch.ExpiresAt,
			&acc.OAuthAccessToken, &acc.OAuthRefreshToken, &acc.OAuthExpiresAt); err != nil {
			return err
		}
		patchString(&acc.UserID, patch.UserID)
		patchString(&acc.Email, patch.Email)
		patchString(&acc.TeamID, patch.TeamID)
		if acc.Name == "" || acc.Name == "grok-device-login" {
			patchString(&acc.Name, patch.Name)
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

// getAccountsByIDs loads the accounts named by the id strings in one MGET,
// decodes them positionally and drops the ids that have no row.
func (s *redisStore) getAccountsByIDs(ctx context.Context, ids []string, onlyEnabled bool) ([]*Account, error) {
	idNums := parseSortedInt64s(ids)
	if len(idNums) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(idNums))
	for _, id := range idNums {
		keys = append(keys, s.accountsKey(id))
	}

	values, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}

	results := make([]*Account, len(values))
	decode := func(i int) error {
		if values[i] == nil {
			return nil
		}
		strVal, ok := values[i].(string)
		if !ok || strVal == "" {
			return fmt.Errorf("invalid account record #%d", idNums[i])
		}
		acc, err := s.unmarshalAccount([]byte(strVal), idNums[i])
		if err != nil {
			return err
		}
		if !onlyEnabled || acc.Enabled {
			results[i] = acc
		}
		return nil
	}
	if err := forEachIndex(len(values), decode); err != nil {
		return nil, err
	}
	return compactNonNil(results), nil
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

func (s *redisStore) accountsKey(id int64) string {
	return s.prefix + "accounts:id:" + strconv.FormatInt(id, 10)
}

func (s *redisStore) accountStatsOperationsKey(id int64) string {
	return s.prefix + "accounts:stats_ops:" + strconv.FormatInt(id, 10)
}

func (s *redisStore) accountsIDsKey() string { return s.prefix + "accounts:ids" }

func (s *redisStore) accountsEnabledKey() string { return s.prefix + "accounts:enabled" }

func (s *redisStore) accountsNextIDKey() string { return s.prefix + "accounts:next_id" }

func (s *redisStore) settingsKey(key string) string { return s.prefix + "settings:" + key }
