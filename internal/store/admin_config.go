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
	backend, ok := s.settings.(*redisStore)
	if !ok {
		return fmt.Errorf("atomic configuration save unavailable")
	}
	keys := []string{backend.settingsKey("config"), backend.prefix + "admin:session_generation"}
	if session != "" {
		sum := sha256.Sum256([]byte(session))
		keys = append(keys, backend.prefix+"admin:sessions:"+hex.EncodeToString(sum[:]))
	}
	result, err := saveConfigWithSessionRevocationScript.Run(ctx, backend.client, keys, value, previous).Int()
	if err == nil && result != 1 {
		return ErrConfigConflict
	}
	return err
}
