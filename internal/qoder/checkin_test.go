package qoder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/provider"
	"orchids-api/internal/testutil"
)

// campaignServer serves the campaigns list and claim endpoints. list returns
// the body for the n-th list call (0-based); claim the body for a claim.
type campaignServer struct {
	mu        sync.Mutex
	lists     int
	claims    []string
	refreshes int
	headers   http.Header
	list      func(n int) (int, string)
	claim     func(id string) (int, string)
}

func (s *campaignServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "deviceToken/refresh"):
			s.refreshes++
			_, _ = w.Write([]byte(`{"device_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`))
		case r.Method == http.MethodGet && r.URL.Path == checkinCampaignsPath:
			s.headers = r.Header.Clone()
			status, body := s.list(s.lists)
			s.lists++
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, checkinCampaignsPath+"/") && strings.HasSuffix(r.URL.Path, "/claim"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, checkinCampaignsPath+"/"), "/claim")
			s.claims = append(s.claims, id)
			status, body := s.claim(id)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func runCheckin(t *testing.T, server *campaignServer) (provider.CheckinResult, error) {
	t.Helper()
	upstream := httptest.NewServer(server.handler(t))
	t.Cleanup(upstream.Close)
	client := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(client, upstream.URL, upstream.URL, upstream.URL)
	return client.Checkin(context.Background())
}

const (
	claimableCredits = `{"data":{"campaigns":[{"campaignId":"daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100}},{"campaignId":"tea","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"REDEMPTION_CODE"}}]}}`
	claimedCredits   = `{"campaigns":[{"campaignId":"daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}]}`
)

func TestCheckinClaimsOnlyCreditCampaigns(t *testing.T) {
	server := &campaignServer{
		list:  func(int) (int, string) { return 200, claimableCredits },
		claim: func(string) (int, string) { return 200, `{"data":{"status":"CLAIMED"}}` },
	}
	result, err := runCheckin(t, server)
	testutil.NoError(t, err)
	testutil.Equal(t, result.Status, provider.CheckinSuccess)
	testutil.Equal(t, result.RewardCredits, 100.0)
	testutil.Equal(t, strings.Join(server.claims, ","), "daily")
	// The device access token is the bearer, and the identity is complete.
	testutil.Equal(t, server.headers.Get("Authorization"), "Bearer access-1")
	testutil.Equal(t, server.headers.Get("Cosy-MachineId"), "11111111-2222-4333-8444-555555555555")
	testutil.Equal(t, server.headers.Get("Cosy-MachineCode"), checkinMachineCode("11111111-2222-4333-8444-555555555555"))
	testutil.Falsef(t, server.headers.Get("Cosy-MachineToken") == "" || server.headers.Get("Cosy-MachineType") == "", "partial device identity: %v", server.headers)
}

func TestCheckinReportsAlreadyAndSkipped(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"already claimed":      {200, claimedCredits, provider.CheckinAlready},
		"no credit campaign":   {200, `{"campaigns":[{"campaignId":"tea","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","benefit":{"kind":"REDEMPTION_CODE"}}]}`, provider.CheckinSkipped},
		"empty list":           {200, `{"data":[]}`, provider.CheckinSkipped},
		"activity not open":    {404, `{}`, provider.CheckinSkipped},
		"other action pending": {200, `{"campaigns":[{"campaignId":"x","actionType":"VISIT_PAGE","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS"}}]}`, provider.CheckinSkipped},
	} {
		t.Run(name, func(t *testing.T) {
			server := &campaignServer{
				list:  func(int) (int, string) { return tc.status, tc.body },
				claim: func(string) (int, string) { t.Error("nothing should be claimed"); return 500, "" },
			}
			result, err := runCheckin(t, server)
			testutil.NoError(t, err)
			testutil.Equal(t, result.Status, tc.want)
		})
	}
}

// The service sometimes records a claim it did not confirm; the relisted
// campaign is the authority.
func TestCheckinConfirmsAnUnconfirmedClaimByRelisting(t *testing.T) {
	server := &campaignServer{
		list: func(n int) (int, string) {
			if n == 0 {
				return 200, claimableCredits
			}
			return 200, claimedCredits
		},
		claim: func(string) (int, string) { return 500, `{"message":"busy"}` },
	}
	result, err := runCheckin(t, server)
	testutil.NoError(t, err)
	testutil.Equal(t, result.Status, provider.CheckinSuccess)

	server = &campaignServer{
		list:  func(int) (int, string) { return 200, claimableCredits },
		claim: func(string) (int, string) { return 200, `{"status":"PENDING"}` },
	}
	_, err = runCheckin(t, server)
	testutil.Falsef(t, err == nil || !strings.Contains(err.Error(), "not confirmed"), "err = %v, want an unconfirmed claim", err)
}

// A refused token (401, or the login redirect the service answers with) is
// refreshed once and the round repeated with the new token.
func TestCheckinRefreshesOnceOnRefusedToken(t *testing.T) {
	for name, refusal := range map[string]int{"401": http.StatusUnauthorized, "login redirect": http.StatusFound} {
		t.Run(name, func(t *testing.T) {
			server := &campaignServer{
				list: func(n int) (int, string) {
					if n == 0 {
						return refusal, `{"code":"TOKEN_EXPIRE"}`
					}
					return 200, claimedCredits
				},
				claim: func(string) (int, string) { return 500, "" },
			}
			result, err := runCheckin(t, server)
			testutil.NoError(t, err)
			testutil.Equal(t, result.Status, provider.CheckinAlready)
			testutil.Equal(t, server.refreshes, 1)
			testutil.Equal(t, server.headers.Get("Authorization"), "Bearer access-2")
		})
	}
}

func TestCheckinDue(t *testing.T) {
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.Local)
	yesterday := now.Add(-24 * time.Hour)
	for name, tc := range map[string]struct {
		enabled bool
		last    time.Time
		status  string
		want    bool
	}{
		"opted out":                {false, time.Time{}, "", false},
		"never checked in":         {true, time.Time{}, "", true},
		"settled today":            {true, now.Add(-time.Hour), provider.CheckinSuccess, false},
		"skipped today":            {true, now.Add(-time.Hour), provider.CheckinSkipped, false},
		"settled yesterday":        {true, yesterday, provider.CheckinAlready, true},
		"failed recently":          {true, now.Add(-10 * time.Minute), provider.CheckinFailed, false},
		"failed an hour ago":       {true, now.Add(-time.Hour), provider.CheckinFailed, true},
		"settled just before 0:00": {true, time.Date(2026, 10, 18, 23, 59, 0, 0, time.Local), provider.CheckinSuccess, true},
	} {
		testutil.Falsef(t, checkinDue(tc.enabled, tc.last, tc.status, now) != tc.want, "%s: due = %v, want %v", name, !tc.want, tc.want)
	}
}
