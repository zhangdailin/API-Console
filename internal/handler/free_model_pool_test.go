package handler

import (
	"context"
	"testing"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestSelectAccountRecord_ExhaustedWorkBuddyRequiresConfirmedAdvertisedFreeModel(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)
	ctx := context.Background()
	acc := &store.Account{
		AccountType: "workbuddy", WorkBuddyRefreshToken: "token", Enabled: true, Weight: 1,
		StatusCode:        store.AccountStatusWorkBuddyQuotaExhausted,
		WorkBuddyModelIDs: []string{`{"id":"future-free-model","credits":"x0.00"}`, `{"id":"gpt-5.6-sol"}`},
	}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	got, err := h.selectAccountRecordWithOptions(ctx, "workbuddy", nil, accountSelectionOptions{ModelID: "future-free-model"})
	testutil.Falsef(t, err != nil || got.ID != acc.ID, "free selection got=%v err=%v", got, err)
	for _, model := range []string{"gpt-5.6-sol", "hy4-preview-f"} {
		_, err := h.selectAccountRecordWithOptions(ctx, "workbuddy", nil, accountSelectionOptions{ModelID: model})
		testutil.CheckError(t, err)
	}
}
func TestSelectAccountRecord_ExhaustedQoderRequiresExplicitAccountFreeFactor(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)
	ctx := context.Background()
	catalog := []string{`{"key":"qfmodel","name":"Qwen3.8-Flash","price_factor":0}`}
	acc := &store.Account{AccountType: "qoder", QoderRefreshToken: "token", Enabled: true, Weight: 1, StatusCode: store.AccountStatusQoderQuotaExhausted, QoderModelIDs: catalog}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	testutil.NoError(t, s.CreateModel(ctx, &store.Model{Channel: "Qoder", ModelID: "qwen3.8-flash", Name: "qwen3.8-flash", Status: store.ModelStatusAvailable, BillingTier: "free", BillingSource: "qoder_price_factor"}))
	got, err := h.selectAccountRecordWithOptions(ctx, "qoder", nil, accountSelectionOptions{ModelID: "qwen3.8-flash"})
	testutil.Falsef(t, err != nil || got.ID != acc.ID, "free selection got=%v err=%v", got, err)
}

func TestQoderBillingLookupIsSharedAcrossCandidatesButNotSelections(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	ctx := context.Background()
	for range 32 {
		acc := &store.Account{AccountType: "qoder", QoderRefreshToken: "test-refresh", Enabled: true, Weight: 1,
			StatusCode:    store.AccountStatusQoderQuotaExhausted,
			QoderModelIDs: []string{`{"key":"free","name":"free","price_factor":0}`}}
		testutil.NoError(t, s.CreateAccount(ctx, acc))
	}
	model := &store.Model{Channel: "Qoder", ModelID: "free", Name: "free", Status: store.ModelStatusAvailable, BillingTier: "free"}
	testutil.NoError(t, s.CreateModel(ctx, model))
	selectAccount := func() error {
		_, err := h.selectAccountRecordWithOptions(ctx, "qoder", nil, accountSelectionOptions{ModelID: "free"})
		return err
	}
	// Warm the account cache so the count isolates model reads.
	testutil.NoError(t, selectAccount())
	before := mini.CommandCount()
	testutil.NoError(t, selectAccount())
	testutil.Equal(t, mini.CommandCount()-before, 2)
	model.BillingTier = "metered"
	testutil.NoError(t, s.UpdateModel(ctx, model))
	before = mini.CommandCount()
	testutil.CheckError(t, selectAccount())
	// Repeated fallback scans must also reuse the negative billing result.
	testutil.Equal(t, mini.CommandCount()-before, 2)
}

// TestSelectAccountRecord_QoderPrefersAccountsWithCredits pins the 2026-11-01
// production finding: spent Free accounts were refused on the free model
// (10605, RESOURCE_LIMIT) while accounts holding credits were admitted. A
// request goes to an account with credits whenever one qualifies, and to a
// spent account only when none does.
func TestSelectAccountRecord_QoderPrefersAccountsWithCredits(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)
	ctx := context.Background()
	catalog := []string{`{"key":"qfmodel","name":"Qwen3.8-Flash","price_factor":0}`}
	testutil.NoError(t, s.CreateModel(ctx, &store.Model{Channel: "Qoder", ModelID: "qwen3.8-flash", Name: "qwen3.8-flash", Status: store.ModelStatusAvailable, BillingTier: "free", BillingSource: "qoder_price_factor"}))
	var spent []int64
	for range 6 {
		acc := &store.Account{AccountType: "qoder", QoderRefreshToken: "token", Enabled: true, Weight: 1, StatusCode: store.AccountStatusQoderQuotaExhausted, QoderModelIDs: catalog}
		testutil.NoError(t, s.CreateAccount(ctx, acc))
		spent = append(spent, acc.ID)
	}
	funded := &store.Account{AccountType: "qoder", QoderRefreshToken: "token", Enabled: true, Weight: 1, QoderModelIDs: catalog,
		QoderQuota: store.QoderQuotaSnapshot{Remaining: 228, Limit: 300}}
	testutil.NoError(t, s.CreateAccount(ctx, funded))

	for range 20 {
		got, err := h.selectAccountRecordWithOptions(ctx, "qoder", nil, accountSelectionOptions{ModelID: "qwen3.8-flash"})
		testutil.NoError(t, err)
		testutil.Equal(t, got.ID, funded.ID)
	}
	got, err := h.selectAccountRecordWithOptions(ctx, "qoder", []int64{funded.ID}, accountSelectionOptions{ModelID: "qwen3.8-flash"})
	testutil.NoError(t, err)
	testutil.True(t, got.ID != funded.ID, "with the funded account excluded a spent one must still serve")
	testutil.True(t, qoderAccountSpent(got), "the fallback must come from the spent accounts")
}
