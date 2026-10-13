package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/billingledger"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// TestHandleKeysCreateBillingLimitEnforcesReservation is the end-to-end admin
// contract: a limit set through the API is what the request path reserves
// against, without any cache or restart in between.
func TestHandleKeysCreateBillingLimitEnforcesReservation(t *testing.T) {
	s, _ := newTestStore(t, "api-keys-billing:")
	a := New(s, "admin", "pass", &config.Config{})
	ctx := t.Context()

	event := billingledger.EventID(time.Now(), "event-a")
	const limit int64 = 5_000_000_000 // 0.50 USD
	createReq := httptest.NewRequest(http.MethodPost, "/api/keys",
		strings.NewReader(fmt.Sprintf(`{"name":"metered","billing_limit_usd_ticks":%d}`, limit)))
	createRec := httptest.NewRecorder()
	a.HandleKeys(createRec, createReq)
	testutil.Equal(t, createRec.Code, http.StatusCreated)
	var created CreateKeyResponse
	testutil.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created), "decode create response: %v")
	testutil.Equal(t, created.BillingLimitUSDTicks, limit)

	// The limit is persisted and visible through the admin list.
	stored, err := s.GetApiKeyByID(ctx, created.ID)
	testutil.NoError(t, err, "GetApiKeyByID() error = %v")
	testutil.Equal(t, stored.BillingLimitUSDTicks, limit)
	listed, err := s.ListApiKeys(ctx)
	testutil.Equal(t, err, nil)
	testutil.Equal(t, len(listed), 1)
	testutil.Equal(t, listed[0].BillingLimitUSDTicks, limit)

	// Reserving inside the limit succeeds; the request that would cross it does
	// not.
	ok, err := s.ReserveApiKeyBilling(ctx, created.ID, event, limit-1, time.Now().UTC().Add(time.Hour))
	testutil.Falsef(t, err != nil || !ok, "reservation inside the limit = %v, %v", ok, err)
	ok, err = s.ReserveApiKeyBilling(ctx, created.ID, "event-b", 2, time.Now().UTC().Add(time.Hour))
	testutil.NoError(t, err, "ReserveApiKeyBilling() error = %v")
	testutil.False(t, ok, "a reservation crossing the limit must be refused")
	// Idempotency: the same event id and amount is the same request, not a
	// second hold.
	ok, err = s.ReserveApiKeyBilling(ctx, created.ID, event, limit-1, time.Now().UTC().Add(time.Hour))
	testutil.Falsef(t, err != nil || !ok, "idempotent re-reserve = %v, %v", ok, err)

	// Settling the request books its cost, and the budget is then exhausted.
	testutil.NoError(t, s.SettleApiKeyBilling(ctx, created.ID, event, limit-1), "SettleApiKeyBilling() error = %v")
	ok, err = s.ReserveApiKeyBilling(ctx, created.ID, "event-c", 2, time.Now().UTC().Add(time.Hour))
	testutil.NoError(t, err, "ReserveApiKeyBilling() error = %v")
	testutil.False(t, ok, "a settled key with no headroom must refuse further reservations")
	// The remaining single tick is still spendable: the limit is a ceiling, not
	// an exclusive bound.
	ok, err = s.ReserveApiKeyBilling(ctx, created.ID, "event-c", 1, time.Now().UTC().Add(time.Hour))
	testutil.Falsef(t, err != nil || !ok, "reservation up to the exact limit = %v, %v", ok, err)
}

// TestHandleKeyBillingLimitUpdateTakesEffect checks the PATCH path, including
// that a billing limit alone counts as a policy change.
func TestHandleKeyBillingLimitUpdateTakesEffect(t *testing.T) {
	s, _ := newTestStore(t, "api-keys-billing-patch:")
	a := New(s, "admin", "pass", &config.Config{})
	ctx := t.Context()

	key := &store.ApiKey{Name: "unlimited", KeyHash: "hash-unlimited", Enabled: true}
	testutil.NoError(t, s.CreateApiKey(ctx, key), "CreateApiKey() error = %v")
	ok, err := s.ReserveApiKeyBilling(ctx, key.ID, "event-unlimited", 1_000_000, time.Now().UTC().Add(time.Hour))
	testutil.Falsef(t, err != nil || !ok, "unlimited reservation = %v, %v", ok, err)
	_, err = s.ReleaseApiKeyBilling(ctx, key.ID, "event-unlimited")
	testutil.CheckNoError(t, err)

	// A PATCH carrying only the billing limit must be accepted.
	patchReq := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/keys/%d", key.ID),
		strings.NewReader(`{"billing_limit_usd_ticks":1000}`))
	patchRec := httptest.NewRecorder()
	a.HandleKeyByID(patchRec, patchReq)
	testutil.Equal(t, patchRec.Code, http.StatusOK)
	var updated store.ApiKey
	testutil.NoError(t, json.Unmarshal(patchRec.Body.Bytes(), &updated), "decode patch response: %v")
	testutil.Equal(t, updated.BillingLimitUSDTicks, 1000)
	ok, err = s.ReserveApiKeyBilling(ctx, key.ID, "event-over", 1001, time.Now().UTC().Add(time.Hour))
	testutil.Falsef(t, err != nil || ok, "reservation above the patched limit = %v, %v", ok, err)
	ok, err = s.ReserveApiKeyBilling(ctx, key.ID, "event-fits", 1000, time.Now().UTC().Add(time.Hour))
	testutil.Falsef(t, err != nil || !ok, "reservation at the patched limit = %v, %v", ok, err)

	// A PATCH with no policy field at all is still rejected.
	emptyReq := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/keys/%d", key.ID), strings.NewReader(`{}`))
	emptyRec := httptest.NewRecorder()
	a.HandleKeyByID(emptyRec, emptyReq)
	testutil.Equal(t, emptyRec.Code, http.StatusBadRequest)
}
