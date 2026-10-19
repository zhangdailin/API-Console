package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// Removing a management action must retire its route and its stored secrets,
// even when a legacy Redis row or client still supplies the retired fields.
func TestRetiredCheckinRoutesAndAccountFields(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	ctx := t.Context()
	acc := &store.Account{AccountType: "qoder", Name: "existing", Enabled: true, QoderAccessToken: "access", QoderRefreshToken: "refresh", QoderMachineID: "session-machine"}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	path := "/api/accounts/" + strconv.FormatInt(acc.ID, 10)
	key := s.RedisPrefix() + "accounts:id:" + strconv.FormatInt(acc.ID, 10)
	raw, err := s.RedisClient().Get(ctx, key).Bytes()
	testutil.NoError(t, err)
	var stored map[string]any
	testutil.NoError(t, json.Unmarshal(raw, &stored))
	retired := map[string]any{
		"auto_checkin":                true,
		"checkin":                     map[string]any{"status": "success", "message": "retired-outcome"},
		"qoder_checkin_machine_token": "retired-token-secret",
		"qoder_checkin_machine_type":  "retired-type-secret",
	}
	for field, value := range retired {
		stored[field] = value
	}
	raw, err = json.Marshal(stored)
	testutil.NoError(t, err)
	testutil.NoError(t, s.RedisClient().Set(ctx, key, raw, 0).Err())
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		a.HandleAccountByID(rec, httptest.NewRequest(method, path+"/checkin", nil))
		testutil.Equal(t, rec.Code, http.StatusNotFound)
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rec := httptest.NewRecorder()
		body := strings.NewReader(`{"account_type":"qoder","name":"edited","enabled":true,"auto_checkin":true,"qoder_checkin_machine_token":"new-retired-token"}`)
		a.HandleAccountByID(rec, httptest.NewRequest(method, path, body))
		testutil.Equal(t, rec.Code, http.StatusOK)
		for field := range retired {
			testutil.MustNotContain(t, rec.Body.String(), `"`+field+`"`)
		}
		testutil.MustNotContain(t, rec.Body.String(), "retired-token")
		testutil.MustNotContain(t, rec.Body.String(), "retired-type")
		testutil.MustNotContain(t, rec.Body.String(), `"checkin_supported"`)
	}
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.QoderRefreshToken, "refresh")
	testutil.Equal(t, got.QoderMachineID, "session-machine")
	raw, err = s.RedisClient().Get(ctx, key).Bytes()
	testutil.NoError(t, err)
	for field := range retired {
		testutil.MustNotContain(t, string(raw), `"`+field+`"`)
	}
}
