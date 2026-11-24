package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

type ApiKey struct {
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	KeyHash         string   `json:"-"`
	ResourceOwner   string   `json:"-"`
	KeyFull         string   `json:"-"`
	KeyPrefix       string   `json:"key_prefix"`
	SecretAvailable bool     `json:"secret_available"`
	KeySuffix       string   `json:"key_suffix"`
	Enabled         bool     `json:"enabled"`
	AllowedModels   []string `json:"allowed_models,omitempty"`
	RPMLimit        int      `json:"rpm_limit,omitempty"`
	MaxConcurrent   int      `json:"max_concurrent,omitempty"`
	// BillingLimitUSDTicks caps how much this key may spend, in USD ticks
	// (1 USD = 10,000,000,000 ticks). Zero means unlimited, so a key created
	// before this field existed keeps working unchanged.
	BillingLimitUSDTicks int64 `json:"billing_limit_usd_ticks,omitempty"`
	// BillingUsedUSDTicks is a read-only projection of the settled usage
	// counter. The Redis counter is authoritative; this field reports it.
	BillingUsedUSDTicks int64 `json:"billing_used_usd_ticks,omitempty"`
	// BillingPeriodDays rolls the settled usage over on a fixed period, the way
	// Settled usage rolls over at the end of a billing period. Zero means the
	// counter only ever moves when an operator resets it.
	BillingPeriodDays int `json:"billing_period_days,omitempty"`
	// BillingPeriodStartedAt is when the current period began. It is written by
	// the rollover, not by the caller.
	BillingPeriodStartedAt time.Time  `json:"billing_period_started_at"`
	ExpiresAt              *time.Time `json:"expires_at,omitempty"`
	LastUsedAt             *time.Time `json:"last_used_at"`
	CreatedAt              time.Time  `json:"created_at"`
}

// rolloverApiKeyBilling atomically advances an elapsed billing period in the
// durable store. The Redis transaction updates the period record and resets only
// settled usage/idempotency state; live holds remain attached to running requests.
func (s *Store) rolloverApiKeyBilling(ctx context.Context, key *ApiKey, now time.Time) {
	if key == nil || key.BillingPeriodDays <= 0 {
		return
	}
	rolled, err := s.advanceApiKeyBillingPeriod(ctx, key, now.UTC())
	if err != nil {
		slog.Warn("failed to roll the billing period over", "key_id", key.ID, "error", err)
		return
	}
	if rolled {
		key.BillingUsedUSDTicks = 0
		key.BillingPeriodStartedAt = now.UTC()
	}
}

// AuthorizeApiKey authenticates a raw client key and atomically applies its
// optional per-minute request limit. Raw keys are never persisted by this path.
func (s *Store) AuthorizeApiKey(ctx context.Context, raw string) (*ApiKey, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("api key store not configured")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrNoRows
	}
	digest := sha256.Sum256([]byte(raw))
	key, err := s.getApiKeyByHash(ctx, hex.EncodeToString(digest[:]))
	if err != nil {
		return nil, err
	}
	if key == nil || !key.Enabled {
		return nil, ErrNoRows
	}
	now := time.Now().UTC()
	if key.ExpiresAt != nil && !now.Before(key.ExpiresAt.UTC()) {
		return nil, ErrApiKeyExpired
	}
	// A key without an explicit per-minute limit has none. A silent 60 RPM
	// default throttled a caller that never asked for a limit; the deployment's
	// admission control is what protects the gateway.
	if rpm := key.RPMLimit; rpm > 0 {
		allowed, err := s.consumeApiKeyRPM(ctx, key.ID, rpm, now)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, ErrApiKeyRateLimited
		}
	}
	key.LastUsedAt = &now
	if key.RPMLimit <= 0 {
		// Persist only the usage touch. Rewriting the stale row here could race an
		// atomic billing rollover and restore its old period start.
		if err := s.touchApiKeyCoalesced(ctx, key.ID, now); err != nil {
			return nil, err
		}
	}
	s.rolloverApiKeyBilling(ctx, key, now)
	return key, nil
}

// Coalesce display-only usage timestamps; policy and billing are still read
// from the authoritative store on every authorization.
func (s *Store) touchApiKeyCoalesced(ctx context.Context, id int64, now time.Time) error {
	s.keyTouchMu.Lock()
	previous := s.keyTouches[id]
	if !previous.IsZero() && now.Sub(previous) < time.Minute {
		s.keyTouchMu.Unlock()
		return nil
	}
	if s.keyTouches == nil || len(s.keyTouches) >= 65536 {
		s.keyTouches = make(map[int64]time.Time)
	}
	s.keyTouches[id] = now
	s.keyTouchMu.Unlock()
	if err := s.touchApiKeyLastUsed(ctx, id, now); err != nil {
		s.keyTouchMu.Lock()
		if s.keyTouches[id].Equal(now) {
			delete(s.keyTouches, id)
		}
		s.keyTouchMu.Unlock()
		return err
	}
	return nil
}
