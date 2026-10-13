package billingledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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
	store   Store
	dir     string
	mu      sync.Mutex
	pending map[string]entry
	diskErr error
}

func New(store Store, dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	l := &Ledger{store: store, dir: dir, pending: make(map[string]entry)}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			return nil, err
		}
		var e entry
		if err = json.Unmarshal(raw, &e); err != nil || e.KeyID <= 0 || e.EventID == "" || e.Amount < 0 || l.name(e) != file.Name() {
			return nil, fmt.Errorf("invalid billing outbox entry %s", file.Name())
		}
		l.pending[file.Name()] = e
	}
	return l, nil
}

func (l *Ledger) name(e entry) string {
	return fmt.Sprintf("%x.json", sha256.Sum256([]byte(fmt.Sprintf("%d:%s", e.KeyID, e.EventID))))
}

// Ready prevents a prolonged outage or full disk from admitting unbounded debt.
func (l *Ledger) Ready() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.diskErr != nil {
		return fmt.Errorf("billing outbox unavailable: %w", l.diskErr)
	}
	if len(l.pending) >= 10000 {
		return fmt.Errorf("billing outbox is full")
	}
	for _, e := range l.pending {
		if _, err := EventExpiry(e.EventID, time.Now()); err != nil {
			return fmt.Errorf("expired billing settlement requires reconciliation")
		}
	}
	return nil
}

func (l *Ledger) queue(e entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	name := l.name(e)
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
	if err := l.store.SettleApiKeyBilling(ctx, id, eventID, amount); err != nil {
		return err
	}
	return l.remove(e)
}

func (l *Ledger) ReleaseApiKeyBilling(ctx context.Context, id int64, eventID string) (bool, error) {
	return l.store.ReleaseApiKeyBilling(ctx, id, eventID)
}

// Replay leaves failures on disk; a receipt whose seven-day window elapsed
// requires operator reconciliation instead of risking a second charge.
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
		_, err := EventExpiry(e.EventID, time.Now())
		if err == nil {
			err = l.store.SettleApiKeyBilling(attempt, e.KeyID, e.EventID, e.Amount)
		}
		cancel()
		if err == nil {
			err = l.remove(e)
		}
		if err != nil {
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
