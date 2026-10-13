package billingledger

import (
	"context"
	"errors"
	"orchids-api/internal/testutil"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type testStore struct {
	mu              sync.Mutex
	seen            map[string]int64
	used            int64
	fail, ambiguous bool
}

func (s *testStore) SettleApiKeyBilling(_ context.Context, _ int64, event string, amount int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("offline")
	}
	if prior, ok := s.seen[event]; ok {
		if prior != amount {
			return errors.New("conflict")
		}
		return nil
	}
	s.seen[event] = amount
	s.used += amount
	if s.ambiguous {
		return context.DeadlineExceeded
	}
	return nil
}
func (*testStore) ReleaseApiKeyBilling(context.Context, int64, string) (bool, error) {
	return false, nil
}

func TestOutboxReplaysAfterRestartWithoutDuplicateCharge(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "offline", true: "committed-response-lost"}[ambiguous], func(t *testing.T) {
			backend := &testStore{seen: map[string]int64{}, fail: !ambiguous, ambiguous: ambiguous}
			dir := t.TempDir()
			ledger, err := New(backend, dir)
			testutil.NoError(t, err)
			event := EventID(time.Now(), "request")
			testutil.Error(t, ledger.SettleApiKeyBilling(context.Background(), 7, event, 300))
			files, err := os.ReadDir(dir)
			testutil.NoError(t, err)
			testutil.Equal(t, len(files), 1)
			restarted, err := New(backend, dir)
			testutil.NoError(t, err)
			backend.fail = false
			backend.ambiguous = false
			restarted.Replay(context.Background())
			restarted.Replay(context.Background())
			testutil.Equal(t, backend.used, int64(300))
			files, err = os.ReadDir(dir)
			testutil.NoError(t, err)
			testutil.Equal(t, len(files), 0)
		})
	}
}
func TestExpiredOutboxRequiresReconciliation(t *testing.T) {
	backend := &testStore{seen: map[string]int64{}, fail: true}
	ledger, err := New(backend, t.TempDir())
	testutil.NoError(t, err)
	testutil.Error(t, ledger.SettleApiKeyBilling(context.Background(), 1, EventID(time.Now().Add(-Retention-time.Hour), "old"), 20))
	backend.fail = false
	ledger.Replay(context.Background())
	testutil.Equal(t, backend.used, int64(0))
	testutil.Error(t, ledger.Ready())
	testutil.Equal(t, len(ledger.pending), 1)
}
func TestOutboxDiskFailureDoesNotClaimSettlement(t *testing.T) {
	backend := &testStore{seen: map[string]int64{}}
	dir := filepath.Join(t.TempDir(), "pending")
	ledger, err := New(backend, dir)
	testutil.NoError(t, err)
	testutil.NoError(t, os.Remove(dir))
	testutil.NoError(t, os.WriteFile(dir, []byte("not a directory"), 0600))
	testutil.Error(t, ledger.SettleApiKeyBilling(context.Background(), 1, EventID(time.Now(), "request"), 20))
	testutil.Equal(t, backend.used, int64(0))
	testutil.Error(t, ledger.Ready())
}

func TestHealthyOutboxSettlementLeavesNoDebt(t *testing.T) {
	backend := &testStore{seen: map[string]int64{}}
	dir := t.TempDir()
	ledger, err := New(backend, dir)
	testutil.NoError(t, err)
	testutil.NoError(t, ledger.SettleApiKeyBilling(t.Context(), 1, EventID(time.Now(), "healthy"), 20))
	testutil.Equal(t, backend.used, int64(20))
	testutil.NoError(t, ledger.Ready())
	files, err := os.ReadDir(dir)
	testutil.NoError(t, err)
	testutil.Equal(t, len(files), 0)
}
