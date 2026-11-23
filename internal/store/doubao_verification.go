package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const doubaoChallengeTTL = 2 * time.Hour

var ErrDoubaoVerificationChanged = errors.New("verification challenge changed or expired")

// DoubaoVerificationChallenge contains only the data needed by the official
// manual verification UI, bound to the persisted account and refused request.
type DoubaoVerificationChallenge struct {
	ID, Decision, DeviceID, Fingerprint, Message string
	AccountVersion, RequestVersion               [32]byte
	RecordedAt, ParkUntil                        time.Time
}

func (s *Store) doubaoVerificationStore(id int64) (*redisStore, string, error) {
	if s != nil && id > 0 {
		if rs, ok := s.accounts.(*redisStore); ok {
			return rs, rs.prefix + "doubao:verification:" + strconv.FormatInt(id, 10), nil
		}
	}
	return nil, "", fmt.Errorf("Doubao verification store is unavailable")
}

func (s *redisStore) readDoubaoVerification(ctx context.Context, cmd redis.Cmdable, key string) (DoubaoVerificationChallenge, error) {
	var challenge DoubaoVerificationChallenge
	wire, err := cmd.Get(ctx, key).Result()
	if err == redis.Nil {
		return challenge, ErrNoRows
	}
	if err != nil {
		return challenge, err
	}
	raw, err := s.credentials.decrypt(wire)
	if err != nil {
		return challenge, err
	}
	err = json.Unmarshal([]byte(raw), &challenge)
	return challenge, err
}

func (s *Store) SaveDoubaoVerification(ctx context.Context, id int64, challenge DoubaoVerificationChallenge) error {
	rs, key, err := s.doubaoVerificationStore(id)
	if err != nil {
		return err
	}
	if rs.credentials == nil {
		return fmt.Errorf("Doubao verification encryption is unavailable")
	}
	if challenge.ID == "" || challenge.Decision == "" || len(challenge.Decision) > 64<<10 || challenge.RecordedAt.IsZero() {
		return fmt.Errorf("invalid Doubao verification challenge")
	}
	payload, err := json.Marshal(challenge)
	if err != nil {
		return err
	}
	sealed, err := rs.credentials.encrypt(string(payload))
	if err != nil {
		return err
	}
	return rs.updateAccountAndRelated(ctx, id, true, []string{key}, func(acc *Account, tx *redis.Tx) (func(redis.Pipeliner), error) {
		if acc.AccountType != "doubao" || !acc.Enabled || !AccountAuthActive(acc) || DoubaoSessionVersion(acc) != challenge.AccountVersion {
			return nil, ErrDoubaoVerificationChanged
		}
		prev, readErr := rs.readDoubaoVerification(ctx, tx, key)
		if readErr == nil && !challenge.RecordedAt.After(prev.RecordedAt) {
			return nil, ErrDoubaoVerificationChanged
		}
		if readErr != nil && !errors.Is(readErr, ErrNoRows) {
			return nil, readErr
		}
		if !acc.LastAttempt.After(challenge.RecordedAt) && !acc.QuotaResetAt.After(challenge.ParkUntil) {
			acc.StatusCode, acc.StatusMessage = "429", challenge.Message
			acc.LastAttempt, acc.QuotaResetAt = challenge.RecordedAt, challenge.ParkUntil
		}
		return func(p redis.Pipeliner) { p.Set(ctx, key, sealed, doubaoChallengeTTL) }, nil
	})
}

func (s *Store) GetDoubaoVerification(ctx context.Context, acc *Account) (DoubaoVerificationChallenge, error) {
	if acc == nil {
		return DoubaoVerificationChallenge{}, ErrNoRows
	}
	rs, key, err := s.doubaoVerificationStore(acc.ID)
	if err != nil {
		return DoubaoVerificationChallenge{}, err
	}
	challenge, err := rs.readDoubaoVerification(ctx, rs.client, key)
	if err == nil && challenge.AccountVersion != DoubaoSessionVersion(acc) {
		err = ErrNoRows
	}
	return challenge, err
}

// AcknowledgeDoubaoVerification clears only the displayed challenge and its
// own park. It leaves credential health and any newer or unrelated hold alone.
func (s *Store) AcknowledgeDoubaoVerification(ctx context.Context, observed *Account, version string) error {
	if observed == nil || version == "" {
		return ErrDoubaoVerificationChanged
	}
	rs, key, err := s.doubaoVerificationStore(observed.ID)
	if err != nil {
		return err
	}
	return rs.updateAccountAndRelated(ctx, observed.ID, true, []string{key}, func(acc *Account, tx *redis.Tx) (func(redis.Pipeliner), error) {
		challenge, err := rs.readDoubaoVerification(ctx, tx, key)
		if errors.Is(err, ErrNoRows) {
			return nil, ErrDoubaoVerificationChanged
		}
		if err != nil {
			return nil, err
		}
		if challenge.ID != version || DoubaoSessionVersion(acc) != challenge.AccountVersion ||
			challenge.AccountVersion != DoubaoSessionVersion(observed) {
			return nil, ErrDoubaoVerificationChanged
		}
		if acc.StatusCode == "429" && acc.StatusMessage == challenge.Message && acc.LastAttempt.Equal(challenge.RecordedAt) {
			acc.StatusCode, acc.StatusMessage = "", ""
			if acc.QuotaResetAt.Equal(challenge.ParkUntil) {
				acc.QuotaResetAt = time.Time{}
			}
		}
		return func(p redis.Pipeliner) { p.Del(ctx, key) }, nil
	})
}
