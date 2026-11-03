package web

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"orchids-api/internal/config"
)

// ConfigureMediaStorage selects the local media root and validates an
// operator-declared shared mount before any media worker starts.
func ConfigureMediaStorage(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("config is required")
	}
	directory := strings.TrimSpace(cfg.MediaDir)
	if directory == "" {
		directory = filepath.Join("data", "tmp")
	}
	directory = filepath.Clean(directory)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create media directory: %w", err)
	}
	for _, kind := range []string{"image", "video"} {
		if err := os.MkdirAll(filepath.Join(directory, kind), 0o755); err != nil {
			return fmt.Errorf("create %s media directory: %w", kind, err)
		}
	}
	cacheBaseDir = directory
	return nil
}

// CacheBaseDir returns the media root the package is writing to, after
// ConfigureMediaStorage has resolved the default and cleaned the configured value.
//
// A caller outside this package needs the resolved path, not the configured one:
// the sweeper walks the same tree the media inputs are written to, and the default
// ("data/tmp" when media_dir is unset) is applied here rather than in the config.
// Before ConfigureMediaStorage runs it returns the package default, which is the
// same value the package would have used.
func CacheBaseDir() string {
	return cacheBaseDir
}
