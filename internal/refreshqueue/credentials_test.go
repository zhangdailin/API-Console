package refreshqueue

import (
	"context"
	"errors"
	"orchids-api/internal/store"
	"testing"
	"time"
)

func TestCredentialGateHandlesTypedNilAndCanceledWaiters(t *testing.T) {
	var missing *store.Store
	if acc, err := LatestAccount(context.Background(), missing, 1); err != nil || acc != nil {
		t.Fatalf("nil store: %v", err)
	}
	release, err := AcquireCredential(context.Background(), "test", missing, 1)
	if err != nil {
		t.Fatal(err)
	}
	release()
	owner := new(int)
	release, err = AcquireCredential(context.Background(), "test", owner, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := AcquireCredential(ctx, "test", owner, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error=%v", err)
	}
	release()
	release()
	credentialGates.Lock()
	defer credentialGates.Unlock()
	if len(credentialGates.entries) != 0 {
		t.Fatal("idle credential gate leaked")
	}
}
