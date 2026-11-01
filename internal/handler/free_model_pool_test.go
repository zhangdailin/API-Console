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
	got, err := h.selectAccountRecord(ctx, "workbuddy", nil, "future-free-model")
	testutil.Falsef(t, err != nil || got.ID != acc.ID, "free selection got=%v err=%v", got, err)
	for _, model := range []string{"gpt-5.6-sol", "hy4-preview-f"} {
		_, err := h.selectAccountRecord(ctx, "workbuddy", nil, model)
		testutil.CheckError(t, err)
	}
}
