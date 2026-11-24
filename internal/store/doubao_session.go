package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DoubaoSessionPatch is a narrow, generation-bound control-plane update. It
// never owns administrator settings, usage counters, or device identity.
type DoubaoSessionPatch struct {
	Expected      [sha256.Size]byte
	Cookie        string
	CookieChanged bool
	Healthy       bool
	Rejected      bool
	UserID        string
	ObservedAt    time.Time
}

// DoubaoSessionObservation contains only a fingerprint and observed metadata,
// never a raw cookie. Account uses it as an internal, non-JSON write intent.
type DoubaoSessionObservation struct {
	Expected          [sha256.Size]byte
	Healthy, Rejected bool
	UserID            string
	ObservedAt        time.Time
}

func DoubaoSessionVersion(acc *Account) [sha256.Size]byte {
	if acc == nil {
		return [sha256.Size]byte{}
	}
	cookie := acc.ClientCookie
	var values map[string]string
	if json.Unmarshal([]byte(cookie), &values) == nil {
		if canonical, err := json.Marshal(values); err == nil {
			cookie = string(canonical)
		}
	}
	raw, _ := json.Marshal([]string{cookie, acc.DoubaoDeviceID, acc.DoubaoWebID, acc.DoubaoTeaUUID})
	return sha256.Sum256(raw)
}

func (s *Store) UpdateDoubaoSession(ctx context.Context, id int64, patch DoubaoSessionPatch) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("doubao session store not configured")
	}
	if patch.Expected == ([sha256.Size]byte{}) || (patch.Healthy && patch.Rejected) || ((patch.Healthy || patch.Rejected) && patch.ObservedAt.IsZero()) {
		return fmt.Errorf("invalid doubao session patch")
	}
	return s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		if !strings.EqualFold(acc.AccountType, "doubao") || DoubaoSessionVersion(acc) != patch.Expected {
			return fmt.Errorf("doubao session changed during update")
		}
		if patch.CookieChanged {
			acc.ClientCookie = patch.Cookie
		}
		if (patch.Healthy || patch.Rejected) && !patch.ObservedAt.Before(acc.VerifiedAt) {
			acc.VerifiedAt = patch.ObservedAt
			if patch.Rejected {
				acc.StatusCode, acc.StatusMessage = "401", "Doubao cookie was rejected; scan again"
				acc.AuthStatus, acc.LastAttempt = AccountAuthStatusReauthRequired, patch.ObservedAt
			} else {
				acc.DoubaoVerifiedAt = patch.ObservedAt
				if patch.UserID != "" {
					acc.UserID = patch.UserID
				}
				// Passport success proves the session, not that a separate chat
				// captcha, restriction, or allowance verdict has been cleared.
				if acc.StatusCode == "401" {
					acc.StatusCode, acc.StatusMessage, acc.AuthStatus = "", "", AccountAuthStatusActive
					acc.LastAttempt, acc.RateLimitFailures = time.Time{}, 0
				}
			}
		}
		return nil
	}, true)
}
