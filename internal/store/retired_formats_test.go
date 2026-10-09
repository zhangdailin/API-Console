package store

import (
	"context"
	"fmt"
	"orchids-api/internal/util"
	"strconv"
	"testing"
	"time"
)

func TestBatchDecodeFailuresAreExplicit(t *testing.T) {
	for _, count := range []int{2, 40} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			s := newBatchReadStore(t)
			ctx := context.Background()
			ids := make([]string, count)
			for i := range count {
				ids[i] = strconv.Itoa(i + 1)
				if err := s.client.Set(ctx, s.accountsKey(int64(i+1)), `{"id":1,"enabled":true}`, 0).Err(); err != nil {
					t.Fatal(err)
				}
				if err := s.client.Set(ctx, s.apiKeysKey(int64(i+1)), `{"id":1,"enabled":true}`, 0).Err(); err != nil {
					t.Fatal(err)
				}
			}
			s.client.Set(ctx, s.accountsKey(int64(count)), `bad-json`, 0)
			s.client.Set(ctx, s.apiKeysKey(int64(count)), `bad-json`, 0)
			if rows, err := s.getAccountsByIDs(ctx, ids, false); err == nil || rows != nil {
				t.Fatalf("accounts returned partial success: %v %v", rows, err)
			}
			if rows, err := s.getApiKeysByIDs(ctx, ids); err == nil || rows != nil {
				t.Fatalf("keys returned partial success: %v %v", rows, err)
			}
			executed := make([]int, count)
			err := forEachIndex(count, func(i int) error {
				executed[i]++
				if i == 0 {
					panic("secret")
				}
				return nil
			})
			if err == nil || err.Error() != util.ErrTaskPanic.Error() {
				t.Fatal(err)
			}
			for _, n := range executed {
				if n != 1 {
					t.Fatal("task skipped or repeated")
				}
			}
		})
	}
}

func TestLegacyReplayIsAbsentWithoutRewritingRedis(t *testing.T) {
	s := newBatchReadStore(t)
	ctx := context.Background()
	key := s.reasoningReplayKey("m", "session")
	old := fmt.Sprintf(`{"model":"m","session_key":"session","encrypted_content":"opaque","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))
	if err := s.client.Set(ctx, key, old, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	row, err := s.GetReasoningReplay(ctx, "m", "session")
	if err != nil || row != nil {
		t.Fatalf("legacy replay remained readable: %#v %v", row, err)
	}
	if got := s.client.Get(ctx, key).Val(); got != old {
		t.Fatal("read rewrote old data")
	}
}

func TestModelWritesDoNotMaintainGlobalIndex(t *testing.T) {
	s := newBatchReadStore(t)
	ctx := context.Background()
	oldKey := s.prefix + "models:model_id_map"
	s.client.HSet(ctx, oldKey, "retired", "old-id")
	m := &Model{Channel: "WorkBuddy", ModelID: "same", Status: ModelStatusAvailable}
	if err := s.CreateModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	m.Name = "updated"
	if err := s.UpdateModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteModel(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	_, err := s.ReconcileDiscoveredModels(ctx, "WorkBuddy", []*Model{{ModelID: "same", Status: ModelStatusAvailable}}, ModelReconcileOptions{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := s.client.HLen(ctx, oldKey).Val(); n != 1 {
		t.Fatal("global index was maintained")
	}
	if got := s.client.HGet(ctx, oldKey, "retired").Val(); got != "old-id" {
		t.Fatal("old data rewritten")
	}
}
