package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// A short-lived device token is rotated while the account still has ID zero.
// The return value, rather than the temporary client's no-op store writes, is
// what the browser flow ultimately persists.
func TestQoderLoginRetainsRotationsAndProfileSnapshot(t *testing.T) {
	refreshes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/deviceToken/refresh":
			refreshes++
			var body struct {
				RefreshToken string `json:"refresh_token"`
			}
			testutil.CheckNoError(t, json.NewDecoder(r.Body).Decode(&body))
			want := "refresh-1"
			if refreshes > 1 {
				want = fmt.Sprintf("refresh-%d", refreshes)
			}
			testutil.CheckEqual(t, body.RefreshToken, want)
			fmt.Fprintf(w, `{"device_token":"access-%d","refresh_token":"refresh-%d","expires_in":3600}`, refreshes+1, refreshes+1)
		case "/api/v1/userinfo":
			testutil.CheckEqual(t, r.Header.Get("Authorization"), "Bearer access-2")
			_, _ = w.Write([]byte(`{"uid":"profile-uid","name":"profile-name","email":"profile@example.com","organization_id":"profile-org","organization_tags":["profile-tag"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a := &API{}
	acc, err := a.buildQoderAccountFromCredentialsWithFactory(t.Context(), "test", "machine-id", qoder.Credentials{
		AccessToken: "access-1", RefreshToken: "refresh-1", AccessExpiresAt: time.Now().Add(30 * time.Second), Name: "poll-name",
	}, qoderLoginConfig(server.URL), qoder.NewFromAccount)
	testutil.NoError(t, err)
	testutil.Falsef(t, refreshes != 1, "refresh calls = %d; catalog/quota should reuse the renewed one-hour credential", refreshes)
	wantAccess := fmt.Sprintf("access-%d", refreshes+1)
	wantRefresh := fmt.Sprintf("refresh-%d", refreshes+1)
	testutil.Falsef(t, acc.QoderAccessToken != wantAccess || acc.QoderRefreshToken != wantRefresh, "returned tokens = %q/%q, want %q/%q", acc.QoderAccessToken, acc.QoderRefreshToken, wantAccess, wantRefresh)
	testutil.Falsef(t, acc.QoderUserID != "profile-uid" || acc.QoderOrganizationID != "profile-org" || acc.QoderUserName != "profile-name" || acc.Email != "profile@example.com" || len(acc.QoderOrganizationTags) != 1 || acc.QoderOrganizationTags[0] != "profile-tag", "profile not returned: %+v", acc)
	testutil.False(t, acc.QoderRuntimeInfo == "" || acc.QoderRuntimeKey == "", "final runtime fields missing")
	s, _ := newTestStore(t, "qoder-rotated-login:")
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	saved, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.Falsef(t, saved.QoderAccessToken != wantAccess || saved.QoderRefreshToken != wantRefresh || saved.QoderRuntimeInfo != acc.QoderRuntimeInfo, "persisted snapshot differs: %+v", saved)
}

func TestVerifyQoderRetainsRotationDuringWholeAccountSave(t *testing.T) {
	refreshes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/deviceToken/refresh":
			refreshes++
			fmt.Fprintf(w, `{"device_token":"access-%d","refresh_token":"refresh-%d","expires_in":86400}`, refreshes+1, refreshes+1)
		case "/api/v1/userinfo":
			testutil.CheckEqual(t, r.Header.Get("Authorization"), "Bearer access-2")
			_, _ = w.Write([]byte(`{"uid":"new-uid","organization_id":"new-org"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	s, _ := newTestStore(t, "qoder-rotated-verify:")
	acc := &store.Account{AccountType: "qoder", Name: "qoder-verify", QoderAccessToken: "access-1", QoderRefreshToken: "refresh-1", QoderExpiresAt: time.Now().Add(time.Minute), QoderMachineID: "machine-id", Enabled: true}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	status, code, err := verifyQoderAccountWithStore(t.Context(), acc, qoderLoginConfig(server.URL), s)
	testutil.Falsef(t, err != nil || status != "" || code != 0, "verify = %q %d %v", status, code, err)
	testutil.Falsef(t, refreshes != 1 || acc.QoderAccessToken != "access-2" || acc.QoderRefreshToken != "refresh-2" || acc.QoderUserID != "new-uid" || acc.QoderOrganizationID != "new-org", "verified snapshot %+v, refreshes=%d", acc, refreshes)
	testutil.NoError(t, s.UpdateAccount(context.Background(), acc))
	stored, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.Falsef(t, stored.QoderRefreshToken != "refresh-2" || stored.QoderAccessToken != "access-2" || stored.QoderUserID != "new-uid", "verification re-saved stale credential: %+v", stored)
}

func TestVerifyQoderQuotaFailurePreservesKnownExhaustion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/userinfo" {
			_, _ = w.Write([]byte(`{"uid":"uid"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	for _, tc := range []struct {
		name   string
		quota  bool
		status string
	}{{"quota-snapshot", true, ""}, {"status-code", false, store.AccountStatusQoderQuotaExhausted}} {
		t.Run(tc.name, func(t *testing.T) {
			acc := &store.Account{AccountType: "qoder", QoderAccessToken: "access", QoderRefreshToken: "refresh", QoderMachineID: "machine", QoderUserID: "uid", StatusCode: tc.status, QoderQuota: store.QoderQuotaSnapshot{Exhausted: tc.quota}}
			status, code, err := verifyQoderAccountWithStore(t.Context(), acc, qoderLoginConfig(server.URL), nil)
			testutil.Falsef(t, err != nil || code != 0 || !strings.EqualFold(status, store.AccountStatusQoderQuotaExhausted), "quota outage = %q %d %v; want exhausted", status, code, err)
		})
	}
}
