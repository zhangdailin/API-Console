package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

func TestRunStartupFailureClosesResources(t *testing.T) {
	// Startup installs process-wide middleware and auth state, so exercise its
	// cleanup in a subprocess without affecting the other route tests.
	if os.Getenv("ORCHIDS_TEST_STARTUP_CLEANUP") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunStartupFailureClosesResources$")
		cmd.Env = append(os.Environ(), "ORCHIDS_TEST_STARTUP_CLEANUP=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("startup cleanup subprocess failed: %v\n%s", err, output)
		}
		return
	}

	t.Setenv("ORCHIDS_CREDENTIAL_ENCRYPTION_KEY", "")
	redis := miniredis.RunT(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	raw, err := json.Marshal(config.Config{
		RedisAddr: redis.Addr(), RedisPrefix: "startup-cleanup:",
		CredentialKeyFile: filepath.Join(dir, "credential.key"),
		MediaDir:          filepath.Join(dir, "state"),
	})
	testutil.NoError(t, err)
	testutil.NoError(t, os.WriteFile(configPath, raw, 0600))
	outbox := filepath.Join(dir, "data", "billing-pending")
	testutil.NoError(t, os.MkdirAll(outbox, 0700))
	testutil.NoError(t, os.WriteFile(filepath.Join(outbox, "invalid.json"), []byte(`{}`), 0600))

	flag.CommandLine = flag.NewFlagSet("startup-cleanup", flag.ContinueOnError)
	os.Args = []string{"orchids-server", "-config", configPath}
	err = run()
	testutil.Error(t, err)
	testutil.MustContain(t, err.Error(), "failed to open billing settlement outbox")

	deadline := time.Now().Add(2 * time.Second)
	for redis.CurrentConnectionCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	testutil.Equal(t, redis.CurrentConnectionCount(), 0)
}
