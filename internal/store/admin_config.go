package store

import (
	"context"
	"fmt"
)

// SaveConfigForCredentials prevents a stale replica's unrelated save from
// restoring credentials that another replica already revoked.
func (s *Store) SaveConfigForCredentials(ctx context.Context, value, password, user string) error {
	backend, ok := s.settings.(*redisStore)
	if !ok {
		return fmt.Errorf("atomic configuration save unavailable")
	}
	return saveConfigWithSessionRevocationScript.Run(ctx, backend.client,
		[]string{backend.settingsKey("config"), backend.prefix + "admin:session_generation"}, value, password, user).Err()
}
