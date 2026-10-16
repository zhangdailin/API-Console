package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestAtomicAccountQualityConcurrentFailuresAndStaleRecovery(t *testing.T) {
	s, _ := newTestRedisStore(t, "quality-atomic:")
	ctx := context.Background()
	acc := &Account{AccountType: "grok", Enabled: true, Name: "keep-name", Weight: 2, OAuthAccessToken: "keep-token"}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	stale, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	const count = 8
	var wg sync.WaitGroup
	errs := make(chan error, count)
	start := make(chan struct{})
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.ApplyAccountQuality(ctx, acc.ID, AccountQualityUpdate{Reason: "no_reasoning", CooldownUntil: time.Now().Add(time.Hour), DisableThreshold: 2})
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	saved, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.QualityFailures != count || saved.QualityRevision != count || saved.Enabled {
		t.Fatalf("lost verdicts: failures=%d rev=%d enabled=%v", saved.QualityFailures, saved.QualityRevision, saved.Enabled)
	}
	result, err := s.ApplyAccountQuality(ctx, acc.ID, AccountQualityUpdate{ExpectedRevision: stale.QualityRevision})
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied || result.Failures != count || result.Enabled {
		t.Fatal("stale healthy response erased a newer failure")
	}
	stale.Weight = 3
	if err := s.UpdateAccount(ctx, stale); err != nil {
		t.Fatal(err)
	}
	saved, err = s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Enabled || saved.QualityFailures != count || saved.OAuthAccessToken != "keep-token" || saved.Name != "keep-name" {
		t.Fatal("stale request overwrote quality verdict or credentials")
	}
	result, err = s.ApplyAccountQuality(ctx, acc.ID, AccountQualityUpdate{ExpectedRevision: saved.QualityRevision})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.Failures != 0 || result.Enabled {
		t.Fatal("current healthy reset must not re-enable an account")
	}
	saved, err = s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved.Enabled = true
	if err := s.UpdateAccount(ctx, saved); err != nil {
		t.Fatal(err)
	}
	saved, err = s.GetAccount(ctx, acc.ID)
	if err != nil || !saved.Enabled {
		t.Fatalf("current admin snapshot cannot enable account: %v", err)
	}
}
