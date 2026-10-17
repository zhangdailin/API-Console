package handler

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

func BenchmarkSelectExhaustedQoderAccounts(b *testing.B) {
	mini := miniredis.RunT(b)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "selection-bench:"})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	for range 32 {
		acc := &store.Account{AccountType: "qoder", QoderRefreshToken: "test-refresh", Enabled: true, Weight: 1,
			StatusCode:    store.AccountStatusQoderQuotaExhausted,
			QoderModelIDs: []string{`{"key":"free","name":"free","price_factor":0}`}}
		if err := s.CreateAccount(ctx, acc); err != nil {
			b.Fatal(err)
		}
	}
	if err := s.CreateModel(ctx, &store.Model{Channel: "Qoder", ModelID: "free", Name: "free", Status: store.ModelStatusAvailable, BillingTier: "free"}); err != nil {
		b.Fatal(err)
	}
	h := NewWithLoadBalancer(nil, loadbalancer.NewWithCacheTTL(s, time.Hour))
	selectAccount := func() {
		if _, err := h.selectAccountRecordWithOptions(ctx, "qoder", nil, accountSelectionOptions{ModelID: "free"}); err != nil {
			b.Fatal(err)
		}
	}
	selectAccount()
	before := mini.CommandCount()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		selectAccount()
	}
	b.StopTimer()
	b.ReportMetric(float64(mini.CommandCount()-before)/float64(b.N), "redis-cmd/op")
}
