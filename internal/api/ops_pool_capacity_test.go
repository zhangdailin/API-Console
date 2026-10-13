package api

import (
	"context"
	"testing"
	"time"

	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/opsagg"
	"orchids-api/internal/store"
)

func TestOpsPoolCapacityUsesAvailableAccountsAndEffectiveLimits(t *testing.T) {
	accounts := []*store.Account{
		{ID: 1, AccountType: "cline", Enabled: true},
		{ID: 2, AccountType: "cline", Enabled: true, MaxConcurrent: 3},
		{ID: 3, AccountType: "cline", Enabled: true, StatusCode: "401", LastAttempt: time.Now()},
		{ID: 4, AccountType: "cline", Enabled: false},
		{ID: 5, AccountType: "qoder", Enabled: true},
	}
	capacity := opsPoolCapacity(accounts, map[int64]int64{1: 2, 2: 1, 3: 4, 4: 5, 5: 6}, "cline")
	if capacity["capacity_slots"] != int64(13) || capacity["occupied_slots"] != int64(3) || capacity["capacity_available"] != true {
		t.Fatalf("wrong eligible capacity: %+v", capacity)
	}
	if _, err := time.Parse(time.RFC3339Nano, capacity["capacity_sampled_at"].(string)); err != nil {
		t.Fatal(err)
	}
}

type failedMonitoringCounts struct {
	*loadbalancer.MemoryConnTracker
}

func (failedMonitoringCounts) GetCountsChecked(context.Context, []int64) (map[int64]int64, error) {
	return nil, context.DeadlineExceeded
}

func TestOpsMatrixPreservesCapacityReadFailure(t *testing.T) {
	a, s, _ := setupConfigAPI(t)
	a.opsAggregator = opsagg.New(s.RedisClient(), "test:")
	a.connTracker = failedMonitoringCounts{loadbalancer.NewMemoryConnTracker()}
	if _, err := a.opsMatrix(context.Background(), []string{"cline"}, time.Now().Add(-time.Minute), time.Now()); err == nil {
		t.Fatal("failed capacity read appeared idle")
	}
	a.connTracker = loadbalancer.NewMemoryConnTracker()
	rows, err := a.opsMatrix(context.Background(), []string{"cline"}, time.Now().Add(-time.Minute), time.Now())
	if err != nil || len(rows) != 1 || rows[0]["capacity_available"] != true || rows[0]["capacity_slots"] != int64(0) {
		t.Fatalf("empty pool: %v %+v", err, rows)
	}
}
