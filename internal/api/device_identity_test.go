package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func deviceIdentityAccount() *store.Account {
	observed := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	return &store.Account{
		AccountType: "doubao", Name: "synthetic identity account", ClientCookie: `{"sessionid":"synthetic-identity-cookie"}`,
		DoubaoDeviceID: "1234567890123456789", DoubaoWebID: "2234567890123456789", DoubaoTeaUUID: "2234567890123456789",
		Enabled: false, AuthStatus: store.AccountAuthStatusReauthRequired,
		StatusCode: "429", StatusMessage: "status=403: 710022004: human verification required",
		DoubaoVerifiedAt: observed, VerifiedAt: observed, LastAttempt: observed,
		QuotaResetAt: observed.Add(time.Hour), UsageCurrent: 3, UsageLimit: 10, UsageTotal: 17,
		RateLimitFailures: 2, Weight: 4, RequestCount: 7,
	}
}

func deviceIdentityRequest(t *testing.T, method string, id int64) *http.Request {
	t.Helper()
	return channelLoginRequest(t, method, "/api/accounts/"+strconv.FormatInt(id, 10)+"/device-identity", "")
}

func assertDeviceIdentityResponseSafe(t *testing.T, response *httptest.ResponseRecorder, accounts ...*store.Account) {
	t.Helper()
	for _, field := range []string{"client_cookie", "sessionid", "doubao_device_id", "doubao_web_id", "doubao_tea_uuid"} {
		testutil.True(t, !strings.Contains(response.Body.String(), field), "response exposed a private identity field")
	}
	for _, acc := range accounts {
		for _, private := range []string{acc.ClientCookie, acc.DoubaoDeviceID, acc.DoubaoWebID, acc.DoubaoTeaUUID} {
			if private != "" {
				testutil.True(t, !strings.Contains(response.Body.String(), private), "response exposed a private identity value")
			}
		}
	}
}

func TestDeviceIdentityRotationPreservesParkAndReturnsOnlyStatus(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			s, _ := newTestStore(t, "device-identity:")
			a := &API{store: s}
			acc := deviceIdentityAccount()
			acc.Enabled = enabled
			testutil.NoError(t, s.CreateAccount(t.Context(), acc))
			before, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)

			rec := httptest.NewRecorder()
			a.HandleAccountByID(rec, deviceIdentityRequest(t, http.MethodPost, acc.ID))
			testutil.Equal(t, rec.Code, http.StatusOK)
			testutil.Equal(t, rec.Header().Get("Cache-Control"), "no-store")
			var out map[string]any
			testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			testutil.Equal(t, len(out), 2)
			testutil.EqualAny(t, out["status"], "updated")
			testutil.EqualAny(t, out["account_id"], float64(acc.ID))

			after, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			testutil.True(t, after.DoubaoDeviceID != before.DoubaoDeviceID && after.DoubaoWebID != before.DoubaoWebID, "rotation did not replace both identities")
			testutil.True(t, after.DoubaoWebID == after.DoubaoTeaUUID, "web identity and tea UUID diverged")
			for _, id := range []string{after.DoubaoDeviceID, after.DoubaoWebID, after.DoubaoTeaUUID} {
				testutil.True(t, len(id) == 19 && id[0] >= '1' && id[0] <= '9', "rotated identity has an invalid shape")
				for i := range len(id) {
					testutil.True(t, id[i] >= '0' && id[i] <= '9', "rotated identity contains a non-digit")
				}
			}
			expected := before.Clone()
			expected.DoubaoDeviceID, expected.DoubaoWebID, expected.DoubaoTeaUUID = after.DoubaoDeviceID, after.DoubaoWebID, after.DoubaoTeaUUID
			expected.UpdatedAt = after.UpdatedAt
			testutil.True(t, reflect.DeepEqual(after, expected), "rotation changed park, enablement, credential, verification, or usage state")
			assertDeviceIdentityResponseSafe(t, rec, before, after)
		})
	}
}

func TestDeviceIdentityRotationRejectsUnsupportedMethodsWithoutChanges(t *testing.T) {
	s, _ := newTestStore(t, "device-identity-method:")
	a := &API{store: s}
	acc := deviceIdentityAccount()
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	before, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			a.HandleAccountByID(rec, deviceIdentityRequest(t, method, acc.ID))
			testutil.Equal(t, rec.Code, http.StatusMethodNotAllowed)
			after, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			testutil.True(t, reflect.DeepEqual(after, before), "unsupported method changed the account")
			assertDeviceIdentityResponseSafe(t, rec, before)
		})
	}
}

func TestDeviceIdentityRotationRejectsOtherProviderWithoutChanges(t *testing.T) {
	s, _ := newTestStore(t, "device-identity-provider:")
	a := &API{store: s}
	acc := deviceIdentityAccount()
	acc.AccountType = "cline"
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	before, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	rec := httptest.NewRecorder()
	a.HandleAccountByID(rec, deviceIdentityRequest(t, http.MethodPost, acc.ID))
	testutil.Equal(t, rec.Code, http.StatusNotFound)
	after, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, reflect.DeepEqual(after, before), "unsupported provider changed the account")
	assertDeviceIdentityResponseSafe(t, rec, before)
}

func TestDeviceIdentityRotationRejectsCrossOriginWithoutChanges(t *testing.T) {
	s, _ := newTestStore(t, "device-identity-origin:")
	a := &API{store: s}
	acc := deviceIdentityAccount()
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	before, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	req := deviceIdentityRequest(t, http.MethodPost, acc.ID)
	req.Header.Set("Origin", "https://foreign.example.invalid")
	rec := httptest.NewRecorder()
	a.HandleAccountByID(rec, req)
	testutil.Equal(t, rec.Code, http.StatusForbidden)
	after, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, reflect.DeepEqual(after, before), "cross-origin request changed the account")
	assertDeviceIdentityResponseSafe(t, rec, before)
}

func TestDeviceIdentityRotationStaleSnapshotReturnsConflict(t *testing.T) {
	for _, changed := range []string{"identity", "cookie"} {
		t.Run(changed, func(t *testing.T) {
			s, _ := newTestStore(t, "device-identity-stale:")
			a := &API{store: s}
			acc := deviceIdentityAccount()
			testutil.NoError(t, s.CreateAccount(t.Context(), acc))
			stale, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			current := stale.Clone()
			if changed == "identity" {
				patch := store.DoubaoIdentityPatch{
					ExpectedCookie: stale.ClientCookie, ExpectedDeviceID: stale.DoubaoDeviceID,
					ExpectedWebID: stale.DoubaoWebID, ExpectedTeaUUID: stale.DoubaoTeaUUID,
					DeviceID: "3234567890123456789", WebID: "4234567890123456789", TeaUUID: "4234567890123456789",
				}
				testutil.NoError(t, s.RotateDoubaoIdentity(t.Context(), acc.ID, patch))
			} else {
				current.ClientCookie = `{"sessionid":"synthetic-refreshed-cookie"}`
				testutil.NoError(t, s.UpdateAccount(t.Context(), current))
			}
			before, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			rec := httptest.NewRecorder()
			// Pass the snapshot already loaded by the dispatcher to make the CAS
			// failure deterministic without racing the request or mutating ports.
			a.handleDeviceIdentityRotation(rec, deviceIdentityRequest(t, http.MethodPost, acc.ID), stale)
			testutil.Equal(t, rec.Code, http.StatusConflict)
			testutil.Equal(t, rec.Header().Get("Cache-Control"), "no-store")
			after, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			testutil.True(t, reflect.DeepEqual(after, before), "stale rotation changed the account")
			assertDeviceIdentityResponseSafe(t, rec, stale, before)
		})
	}
}
