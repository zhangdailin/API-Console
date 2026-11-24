package billingledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrSettlementKeyMissing means Redis could not settle an event for a deleted key.
var ErrSettlementKeyMissing = errors.New("billing settlement key no longer exists")

var errReconciliationRequired = errors.New("billing settlement requires reconciliation")

const maxPendingSettlements = 10000

type Store interface {
	SettleApiKeyBilling(context.Context, int64, string, int64) error
	ReleaseApiKeyBilling(context.Context, int64, string) (bool, error)
}

type entry struct {
	KeyID   int64  `json:"key_id"`
	EventID string `json:"event_id"`
	Amount  int64  `json:"amount_ticks"`
}

// Ledger records settlement intent on disk before touching Redis. Ambiguous
// writes replay with the same identity after a disconnect or process restart.
// Keep this directory on persistent storage alongside the credential key.
type Ledger struct {
	store      Store
	dir        string
	mu         sync.Mutex
	pending    map[string]entry
	review     map[string]entry
	reviewKeys map[int64]bool
	diskErr    error
}

func New(store Store, dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	l := &Ledger{store: store, dir: dir, pending: make(map[string]entry), review: make(map[string]entry), reviewKeys: make(map[int64]bool)}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		name, archived := strings.CutSuffix(file.Name(), ".reconcile")
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			return nil, err
		}
		var e entry
		if err = json.Unmarshal(raw, &e); err != nil || e.KeyID <= 0 || e.EventID == "" || e.Amount < 0 || l.name(e) != name {
			return nil, fmt.Errorf("invalid billing outbox entry %s", file.Name())
		}
		if archived {
			l.review[name] = e
			l.reviewKeys[e.KeyID] = true
		} else {
			l.pending[name] = e
		}
	}
	return l, nil
}

func (l *Ledger) name(e entry) string {
	return fmt.Sprintf("%x.json", sha256.Sum256([]byte(fmt.Sprintf("%d:%s", e.KeyID, e.EventID))))
}

// ReadyForKey rejects new debt for a key under review without affecting other keys.
// Disk failures and a full active queue still stop all admissions.
func (l *Ledger) ReadyForKey(keyID int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.diskErr != nil {
		return fmt.Errorf("billing outbox unavailable: %w", l.diskErr)
	}
	if len(l.pending) >= maxPendingSettlements {
		return fmt.Errorf("billing outbox is full")
	}
	if l.reviewKeys[keyID] {
		return errReconciliationRequired
	}
	now := time.Now()
	for _, e := range l.pending {
		if e.KeyID == keyID {
			if _, err := EventExpiry(e.EventID, now); err != nil {
				return errReconciliationRequired
			}
		}
	}
	return nil
}

func (l *Ledger) queue(e entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	name := l.name(e)
	if _, ok := l.review[name]; ok {
		return errReconciliationRequired
	}
	if prior, ok := l.pending[name]; ok {
		if prior != e {
			return fmt.Errorf("conflicting billing settlement")
		}
		return nil
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(l.dir, ".pending-")
	if err != nil {
		l.diskErr = err
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = persistPendingFile(file.Name(), filepath.Join(l.dir, name), l.dir)
	}
	if err != nil {
		l.diskErr = err
		return err
	}
	l.pending[name] = e
	l.diskErr = nil
	return nil
}

// archive removes an unreplayable event from automatic retry, retaining its evidence.
func (l *Ledger) archive(e entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	name := l.name(e)
	if _, ok := l.pending[name]; !ok {
		return nil
	}
	source := filepath.Join(l.dir, name)
	target := source + ".reconcile"
	if err := persistPendingFile(source, target, l.dir); err != nil {
		if _, statErr := os.Stat(target); statErr != nil || syncRemovedEntry(l.dir) != nil {
			l.diskErr = err
			return err
		}
	}
	delete(l.pending, name)
	l.review[name] = e
	l.reviewKeys[e.KeyID] = true
	return nil
}

func (l *Ledger) remove(e entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	name := l.name(e)
	if _, ok := l.pending[name]; !ok {
		return nil
	}
	if err := os.Remove(filepath.Join(l.dir, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := syncRemovedEntry(l.dir); err != nil {
		return err
	}
	delete(l.pending, name)
	return nil
}

func (l *Ledger) SettleApiKeyBilling(ctx context.Context, id int64, eventID string, amount int64) error {
	e := entry{id, eventID, amount}
	if err := l.queue(e); err != nil {
		return err
	}
	return l.reconcile(ctx, e)
}

func (l *Ledger) ReleaseApiKeyBilling(ctx context.Context, id int64, eventID string) (bool, error) {
	return l.store.ReleaseApiKeyBilling(ctx, id, eventID)
}

func (l *Ledger) reconcile(ctx context.Context, e entry) error {
	_, err := EventExpiry(e.EventID, time.Now())
	if err == nil {
		err = l.store.SettleApiKeyBilling(ctx, e.KeyID, e.EventID, e.Amount)
		if err == nil {
			return l.remove(e)
		}
		if !errors.Is(err, ErrSettlementKeyMissing) {
			return err
		}
	}
	if archiveErr := l.archive(e); archiveErr != nil {
		return archiveErr
	}
	slog.Warn("Billing settlement archived for reconciliation", "key_id", e.KeyID, "event_id", e.EventID, "reason", err)
	return errReconciliationRequired
}

// Replay retries transient failures and archives events that cannot be retried safely.
func (l *Ledger) Replay(ctx context.Context) {
	l.mu.Lock()
	pending := make([]entry, 0, len(l.pending))
	for _, e := range l.pending {
		pending = append(pending, e)
	}
	l.mu.Unlock()
	for _, e := range pending {
		if ctx.Err() != nil {
			return
		}
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := l.reconcile(attempt, e)
		cancel()
		if err != nil && !errors.Is(err, errReconciliationRequired) {
			slog.Error("Billing reconciliation pending", "key_id", e.KeyID, "event_id", e.EventID, "error", err)
		}
	}
}

func (l *Ledger) Run(ctx context.Context) {
	l.Replay(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.Replay(ctx)
		}
	}
}
