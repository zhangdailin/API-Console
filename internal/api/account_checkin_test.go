package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// The check-in action exists only where the channel registered one, is a
// POST, and the account output says which accounts offer it.
func TestAccountCheckinRouteFollowsChannelCapability(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	ctx := context.Background()

	grok := &store.Account{AccountType: "grok", CredentialType: "oauth", OAuthAccessToken: "a", OAuthRefreshToken: "r", OAuthExpiresAt: time.Now().Add(time.Hour), Enabled: true, Name: "g"}
	testutil.NoError(t, s.CreateAccount(ctx, grok))
	rec := httptest.NewRecorder()
	a.HandleAccountByID(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/"+strconv.FormatInt(grok.ID, 10)+"/checkin", nil))
	testutil.Equal(t, rec.Code, http.StatusNotFound)

	qoder := &store.Account{AccountType: "qoder", QoderAccessToken: "access", QoderRefreshToken: "refresh", QoderMachineID: "11111111-2222-4333-8444-555555555555", Enabled: true, Name: "q"}
	testutil.NoError(t, s.CreateAccount(ctx, qoder))
	rec = httptest.NewRecorder()
	a.HandleAccountByID(rec, httptest.NewRequest(http.MethodGet, "/api/accounts/"+strconv.FormatInt(qoder.ID, 10)+"/checkin", nil))
	testutil.Equal(t, rec.Code, http.StatusMethodNotAllowed)

	for id, want := range map[int64]bool{grok.ID: false, qoder.ID: true} {
		rec = httptest.NewRecorder()
		a.HandleAccountByID(rec, httptest.NewRequest(http.MethodGet, "/api/accounts/"+strconv.FormatInt(id, 10), nil))
		testutil.Equal(t, rec.Code, http.StatusOK)
		var out map[string]any
		testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		testutil.EqualAny(t, out["checkin_supported"], want)
	}
}
