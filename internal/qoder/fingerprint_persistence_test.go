package qoder

import (
	"context"
	"errors"
	"testing"
)

type failingSaltSettings struct {
	stored            string
	readErr, writeErr error
	reads, writes     int
	saltDuringWrite   string
}

func (s *failingSaltSettings) GetSetting(context.Context, string) (string, error) {
	s.reads++
	return s.stored, s.readErr
}

func (s *failingSaltSettings) SetSetting(_ context.Context, _, value string) error {
	s.writes++
	s.saltDuringWrite = InstallSalt()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.stored = value
	return nil
}

func TestEnsureInstallSaltReadFailureNeverOverwrites(t *testing.T) {
	previous := InstallSalt()
	t.Cleanup(func() { SetInstallSalt(previous) })
	SetInstallSalt("")
	baseline := FingerprintFor("machine", "uid", "token")
	s := &failingSaltSettings{stored: "persisted-identity", readErr: errors.New("store unavailable")}
	for i := 0; i < 2; i++ {
		if got := EnsureInstallSalt(context.Background(), s); got != "" || InstallSalt() != "" {
			t.Fatalf("read failure activated salt %q", got)
		}
		if got := FingerprintFor("machine", "uid", "token"); got != baseline {
			t.Fatalf("fallback fingerprint drifted: %+v", got)
		}
	}
	if s.writes != 0 || s.stored != "persisted-identity" {
		t.Fatalf("read failure overwrote persisted salt: %+v", s)
	}
	s.readErr = nil
	if got := EnsureInstallSalt(context.Background(), s); got != "persisted-identity" {
		t.Fatalf("recovery salt = %q", got)
	}
	if s.writes != 0 {
		t.Fatalf("recovery rewrote existing salt")
	}
}

func TestEnsureInstallSaltWriteFailureDoesNotActivateEphemeralSalt(t *testing.T) {
	previous := InstallSalt()
	t.Cleanup(func() { SetInstallSalt(previous) })
	SetInstallSalt("")
	baseline := FingerprintFor("machine", "uid", "token")
	s := &failingSaltSettings{writeErr: errors.New("read-only store")}
	for i := 0; i < 2; i++ {
		SetInstallSalt("") // simulate a restart after the failed persistence
		if got := EnsureInstallSalt(context.Background(), s); got != "" || InstallSalt() != "" {
			t.Fatalf("write failure activated salt %q", got)
		}
		if got := FingerprintFor("machine", "uid", "token"); got != baseline {
			t.Fatalf("failed-write restart drifted: %+v", got)
		}
	}
	if s.saltDuringWrite != "" || s.stored != "" {
		t.Fatalf("salt activated before durable write: %+v", s)
	}
	s.writeErr = nil
	first := EnsureInstallSalt(context.Background(), s)
	if first == "" || s.stored != first || InstallSalt() != first || s.saltDuringWrite != "" {
		t.Fatalf("successful persistence not activated correctly: %+v", s)
	}
	SetInstallSalt("")
	if got := EnsureInstallSalt(context.Background(), s); got != first {
		t.Fatalf("durable salt changed after restart: %q != %q", got, first)
	}
}

func TestEnsureInstallSaltKeepsLoadedIdentityDuringStoreFailure(t *testing.T) {
	previous := InstallSalt()
	t.Cleanup(func() { SetInstallSalt(previous) })
	SetInstallSalt("already-loaded")
	before := FingerprintFor("machine", "uid", "token")
	s := &failingSaltSettings{readErr: errors.New("offline"), writeErr: errors.New("offline")}
	if got := EnsureInstallSalt(context.Background(), s); got != "already-loaded" {
		t.Fatalf("loaded salt lost: %q", got)
	}
	if s.reads != 0 || s.writes != 0 || FingerprintFor("machine", "uid", "token") != before {
		t.Fatal("loaded identity should not consult a failing store or change")
	}
}
