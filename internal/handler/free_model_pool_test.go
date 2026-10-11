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
