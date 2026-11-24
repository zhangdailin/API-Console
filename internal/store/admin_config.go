package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

var ErrConfigConflict = errors.New("configuration changed concurrently")

// CompareAndSaveConfig compares the exact Redis preimage and, for browser
// requests, rechecks the session generation in the same transaction.
func (s *Store) CompareAndSaveConfig(ctx context.Context, previous, value, session string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("atomic configuration save unavailable")
	}
	keys := []string{s.settingsKey("config"), s.prefix + "admin:session_generation"}
	if session != "" {
		sum := sha256.Sum256([]byte(session))
		keys = append(keys, s.prefix+"admin:sessions:"+hex.EncodeToString(sum[:]))
	}
	result, err := saveConfigWithSessionRevocationScript.Run(ctx, s.client, keys, value, previous).Int()
	if err == nil && result != 1 {
		return ErrConfigConflict
	}
	return err
}
