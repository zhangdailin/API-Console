package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

func TestOpsPoolsReadOnlyScopeAndCapacity(t *testing.T) {
	a, s, _ := setupConfigAPI(t)
	tracker := loadbalancer.NewMemoryConnTracker()
	a.connTracker = tracker
	ctx := context.Background()
	accounts := []*store.Account{
		{AccountType: "cline", Name: "active", Enabled: true, MaxConcurrent: 4, ClineAccessToken: "must-not-leak", ClineRefreshToken: "never-refresh"},
		{AccountType: "cline", Name: "held", Enabled: true, StatusCode: "401", LastAttempt: time.Now()},
		{AccountType: "cline", Name: "disabled", Enabled: false},
		{AccountType: "qoder", Name: "other", Enabled: true},
	}
	for _, acc := range accounts {
		if err := s.CreateAccount(ctx, acc); err != nil {
			t.Fatal(err)
		}
	}
	tracker.Acquire(accounts[0].ID)
	tracker.Acquire(accounts[1].ID)
	read := func(path string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		a.HandleOpsPools(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("capacity response may be cached")
		}
		return result
	}
	all := read("/api/ops/pools")
	if len(all["rows"].([]any)) != 4 {
		t.Fatal("all providers must retain stable rows")
	}
	scoped := read("/api/ops/pools?channel=cline")
	rows := scoped["rows"].([]any)
	row := rows[0].(map[string]any)
	if len(rows) != 1 || row["channel"] != "cline" || row["accounts_enabled"] != float64(2) || row["accounts_available"] != float64(1) || row["capacity_slots"] != float64(4) || row["occupied_slots"] != float64(1) || row["active_requests"] != float64(2) {
		t.Fatalf("wrong pool counts: %+v", row)
	}
	for _, key := range []string{"sampled_at"} {
		if _, err := time.Parse(time.RFC3339Nano, scoped[key].(string)); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := s.GetAccount(ctx, accounts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ClineAccessToken != "must-not-leak" || saved.ClineRefreshToken != "never-refresh" {
		t.Fatal("monitoring changed credentials")
	}
	raw, _ := json.Marshal(scoped)
	if len(raw) == 0 {
		t.Fatal("empty response")
	}
	for _, secret := range []string{"must-not-leak", "never-refresh"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("pool endpoint exposed credentials")
		}
	}
	for _, r := range rows {
		for key := range r.(map[string]any) {
			if key == "name" || key == "id" || key == "cline_access_token" {
				t.Fatal("pool endpoint exposed account identity")
			}
		}
	}
}

func TestOpsPoolsFailuresAreNotIdleCapacity(t *testing.T) {
	a, _, _ := setupConfigAPI(t)
	cases := []struct {
		method, path string
		status       int
	}{{"POST", "/api/ops/pools", 405}, {"GET", "/api/ops/pools?channel=unknown", 400}}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		a.HandleOpsPools(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.status {
			t.Fatalf("status %d expected %d", rec.Code, c.status)
		}
	}
	a.connTracker = failedMonitoringCounts{loadbalancer.NewMemoryConnTracker()}
	rec := httptest.NewRecorder()
	a.HandleOpsPools(rec, httptest.NewRequest("GET", "/api/ops/pools", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatal("failed Redis capacity read looked idle")
	}
	a.connTracker = nil
	rec = httptest.NewRecorder()
	a.HandleOpsPools(rec, httptest.NewRequest("GET", "/api/ops/pools", nil))
	var result struct {
		Rows []struct {
			CapacityAvailable    bool `json:"capacity_available"`
			ConcurrencyAvailable bool `json:"concurrency_available"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || len(result.Rows) != 4 {
		t.Fatal("missing tracker must retain account counts")
	}
	for _, row := range result.Rows {
		if row.CapacityAvailable || row.ConcurrencyAvailable {
			t.Fatal("unknown capacity became zero capacity")
		}
	}
	rec = httptest.NewRecorder()
	(&API{}).HandleOpsPools(rec, httptest.NewRequest("GET", "/api/ops/pools", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatal("missing store looked like a healthy empty pool")
	}
}
