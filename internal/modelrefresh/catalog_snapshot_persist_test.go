package modelrefresh

import (
	"context"
	"reflect"
	"testing"
	"time"

	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestPersistAccountCatalogSnapshotChannels(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	tests := []struct {
		channel string
		ids     func(*store.Account) []string
		synced  func(*store.Account) time.Time
	}{
		{"qoder", func(a *store.Account) []string { return a.QoderModelIDs }, func(a *store.Account) time.Time { return a.QoderModelsSyncedAt }},
		{"workbuddy", func(a *store.Account) []string { return a.WorkBuddyModelIDs }, func(a *store.Account) time.Time { return a.WorkBuddyModelsSyncedAt }},
		{"cline", func(a *store.Account) []string { return a.ClineModelIDs }, func(a *store.Account) time.Time { return a.ClineModelsSyncedAt }},
	}
	for _, tt := range tests {
		t.Run(tt.channel, func(t *testing.T) {
			acc := &store.Account{Name: tt.channel, AccountType: tt.channel, Enabled: true}
			testutil.NoError(t, s.CreateAccount(ctx, acc))
			before := time.Now()
			want := []string{"model-a", "model-b"}
			PersistAccountCatalogSnapshot(ctx, s, acc, tt.channel, want, "test update failure")
			got, err := s.GetAccount(ctx, acc.ID)
			testutil.NoError(t, err)
			testutil.Falsef(t, !reflect.DeepEqual(tt.ids(got), want), "persisted ids = %v; want %v", tt.ids(got), want)
			stamp := tt.synced(got)
			testutil.Falsef(t, stamp.Before(before) || stamp.After(time.Now()), "persisted timestamp = %v, not within refresh interval", stamp)
			// An empty observation must preserve the in-memory and stored last-known-good snapshot and timestamp.
			for _, empty := range [][]string{nil, {}} {
				PersistAccountCatalogSnapshot(ctx, s, acc, tt.channel, empty, "test update failure")
				after, err := s.GetAccount(ctx, acc.ID)
				testutil.NoError(t, err)
				if !reflect.DeepEqual(tt.ids(after), want) || !tt.synced(after).Equal(stamp) ||
					!reflect.DeepEqual(tt.ids(acc), want) || !tt.synced(acc).Equal(stamp) {
					t.Fatalf("empty observation changed snapshot: stored=%v/%v in-memory=%v/%v", tt.ids(after), tt.synced(after), tt.ids(acc), tt.synced(acc))
				}
			}
		})
	}
}

func TestPersistAccountCatalogSnapshotZeroIDPreservesAutoBehavior(t *testing.T) {
	// The automatic entry points have never filtered zero IDs: UpdateAccount is
	// a no-op for such rows, but the in-memory snapshot still advances.
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	acc := &store.Account{}
	PersistAccountCatalogSnapshot(context.Background(), s, acc, "cline", []string{"m"}, "test update failure")
	testutil.Falsef(t, !reflect.DeepEqual(acc.ClineModelIDs, []string{"m"}) || acc.ClineModelsSyncedAt.IsZero(), "zero-ID automatic snapshot was not mutated: %+v", acc)
	// Manual discovery keeps its pre-existing zero-ID guard.
	manual := &store.Account{}
	for _, channel := range []string{"qoder", "workbuddy", "cline"} {
		spec, ok := provider.Lookup(channel)
		testutil.True(t, ok, "channel %q is not registered")
		observation := provider.Observation{Record: provider.RecordSnapshot([]string{"m"}, spec.Catalog.SetSnapshot)}
		testutil.NoError(t, persistObservation(context.Background(), nil, manual, observation, time.Now()))
	}
	testutil.Falsef(t, !manual.QoderModelsSyncedAt.IsZero() || !manual.WorkBuddyModelsSyncedAt.IsZero() || !manual.ClineModelsSyncedAt.IsZero(), "zero-ID manual discovery mutated the account: %+v", manual)
}
