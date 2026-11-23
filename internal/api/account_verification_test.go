package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestAccountVerificationEndpointAcrossReplicas(t *testing.T) {
	a, s, mini := setupConfigAPI(t)
	cfg := a.ConfigSnapshot()
	b := New(s, cfg.AdminUser, cfg.AdminPass, cfg)
	call := func(api *API, method string, acc *store.Account, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		api.HandleAccountByID(rec, httptest.NewRequest(method, "/api/accounts/"+strconv.FormatInt(acc.ID, 10)+"/verification", strings.NewReader(body)))
		return rec
	}
	ctx := t.Context()
	cline := &store.Account{AccountType: "cline", Name: "c", ClineRefreshToken: "r", Enabled: true}
	testutil.NoError(t, s.CreateAccount(ctx, cline))
	rec := call(a, http.MethodGet, cline, "")
	testutil.Equal(t, rec.Code, http.StatusNotFound)
	testutil.MustContain(t, rec.Body.String(), "no verification challenge")

	parkedAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	verifiedAt := parkedAt.Add(-time.Hour)
	acc := &store.Account{AccountType: "doubao", Name: "d", ClientCookie: `{"sessionid":"x"}`, DoubaoDeviceID: "7500000000000000002", Enabled: true, VerifiedAt: verifiedAt}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	rec = call(a, http.MethodGet, acc, "")
	testutil.Equal(t, rec.Code, http.StatusNotFound)
	testutil.MustContain(t, rec.Body.String(), "no recent verification challenge")
	testutil.MustNotContain(t, rec.Body.String(), acc.DoubaoDeviceID)
	testutil.Equal(t, call(a, http.MethodDelete, acc, "").Code, http.StatusMethodNotAllowed)
	testutil.Equal(t, call(a, http.MethodPost, acc, `{}`).Code, http.StatusBadRequest)

	challenge := func(version string, at time.Time) store.DoubaoVerificationChallenge {
		return store.DoubaoVerificationChallenge{
			ID: version, Decision: "synthetic-decision", DeviceID: acc.DoubaoDeviceID,
			AccountVersion: store.DoubaoSessionVersion(acc),
			Message:        "Doubao account requires human verification",
			RecordedAt:     at, ParkUntil: at.Add(accountpolicy.CooldownVerification),
		}
	}
	first := challenge("first", parkedAt)
	testutil.NoError(t, s.SaveDoubaoVerification(ctx, acc.ID, first))
	rec = call(b, http.MethodGet, acc, "")
	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.Equal(t, rec.Header().Get("Cache-Control"), "no-store")
	var shown struct{ Version string }
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &shown))
	testutil.Equal(t, shown.Version, first.ID)
	testutil.MustNotContain(t, rec.Body.String(), "sessionid")

	second := challenge("second", parkedAt.Add(time.Second))
	testutil.NoError(t, s.SaveDoubaoVerification(ctx, acc.ID, second))
	testutil.Equal(t, call(b, http.MethodPost, acc, `{"version":"first"}`).Code, http.StatusConflict)
	saved, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, saved.QuotaResetAt.Equal(second.ParkUntil), "stale confirmation cleared a newer park")
	rec = call(b, http.MethodPost, acc, `{"version":"second"}`)
	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.MustNotContain(t, rec.Body.String(), "sessionid")
	saved, err = s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, saved.StatusCode, "")
	testutil.True(t, saved.QuotaResetAt.IsZero())
	testutil.True(t, saved.VerifiedAt.Equal(verifiedAt), "answering a challenge must not claim a credential check")
	testutil.Equal(t, call(a, http.MethodGet, acc, "").Code, http.StatusNotFound)

	// An unrelated hold created after the observed refusal stays in place.
	third := challenge("third", parkedAt.Add(2*time.Second))
	testutil.NoError(t, s.SaveDoubaoVerification(ctx, acc.ID, third))
	saved, err = s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	saved.StatusCode, saved.StatusMessage, saved.LastAttempt = "429", "upstream rate limited", parkedAt.Add(3*time.Second)
	testutil.NoError(t, s.UpdateAccount(ctx, saved))
	testutil.Equal(t, call(a, http.MethodPost, acc, `{"version":"third"}`).Code, http.StatusOK)
	saved, err = s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, saved.StatusMessage, "upstream rate limited")

	// The challenge payload is encrypted and its lookup expires independently
	// of the account row or the replica serving the admin request.
	fourth := challenge("fourth", parkedAt.Add(4*time.Second))
	testutil.NoError(t, s.SaveDoubaoVerification(ctx, acc.ID, fourth))
	wire, err := mini.Get(s.RedisPrefix() + "doubao:verification:" + strconv.FormatInt(acc.ID, 10))
	testutil.NoError(t, err)
	testutil.MustNotContain(t, wire, fourth.Decision)
	mini.FastForward(2 * time.Hour)
	testutil.Equal(t, call(b, http.MethodGet, acc, "").Code, http.StatusNotFound)
	testutil.Equal(t, call(b, http.MethodPost, acc, `{"version":"fourth"}`).Code, http.StatusConflict)
}
