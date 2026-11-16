package warp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/store"
)

const quotaTestReset = "2027-01-02T03:04:05Z"

func quotaTestResponse(workspaces, legacy string) string {
	return `{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":` + workspaces + `,"requestLimitInfo":` + legacy + `}}}}`
}

func quotaTestWorkspace(limit, used string) string {
	return `{"includedUsageInfo":{"includedUsageCredits":` + limit + `,"usageCreditsUsedSinceLastRefresh":` + used + `,"nextRefreshTime":"` + quotaTestReset + `"}}`
}

func quotaTestClient(t *testing.T, response string) *Client {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(s.Close)
	return NewFromAccount(testAccount(), testConfig(s.URL))
}

func quotaAssertNumber(t *testing.T, actual *float64, expected float64) {
	t.Helper()
	if actual == nil || *actual != expected {
		t.Fatalf("measurement = %v, want %v", actual, expected)
	}
}

func TestFetchQuotaMinimalOfficialQuery(t *testing.T) {
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/graphql/v2" || r.URL.RawQuery != "op=GetRequestLimitInfo" {
			t.Error("unexpected quota endpoint")
		}
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Error("unexpected quota request headers")
		}
		var payload struct {
			Query         string         `json:"query"`
			OperationName string         `json:"operationName"`
			Variables     map[string]any `json:"variables"`
		}
		if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.OperationName != "GetRequestLimitInfo" || payload.Query != quotaQuery {
			t.Error("unexpected quota operation")
		}
		for _, field := range []string{"__typename", "UserOutput", "workspaces", "includedUsageInfo", "includedUsageCredits", "usageCreditsUsedSinceLastRefresh", "requestLimitInfo", "isUnlimited", "requestsUsedSinceLastRefresh", "requestLimit", "nextRefreshTime"} {
			if !strings.Contains(payload.Query, field) {
				t.Errorf("missing quota field %s", field)
			}
		}
		for _, field := range []string{"uid", " name ", "email", "bonus", "Cents", "requestLimitRefreshDuration"} {
			if strings.Contains(payload.Query, field) {
				t.Errorf("unexpected identifying or unused field %s", field)
			}
		}
		if payload.Variables["requestContext"] == nil {
			t.Error("missing request context")
		}
		_, _ = io.WriteString(w, quotaTestResponse("["+quotaTestWorkspace("100", "25")+"]", "null"))
	}))
	defer s.Close()
	c := NewFromAccount(testAccount(), testConfig(s.URL))
	before := *c.account
	snapshot, err := c.FetchQuota(context.Background())
	if err != nil || snapshot.Status != "ok" || snapshot.Source != quotaSource || len(snapshot.Windows) != 1 || calls.Load() != 1 {
		t.Fatalf("unexpected snapshot: %+v, error: %v", snapshot, err)
	}
	if snapshot.AttemptedAt.IsZero() || !snapshot.SyncedAt.Equal(snapshot.AttemptedAt) {
		t.Fatal("missing observation timestamps")
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("observation violates store contract: %v", err)
	}
	window := snapshot.Windows[0]
	if window.Scope != "workspace_monthly" || window.Unit != "credits" || window.HasUnlimited || window.Unlimited || window.BonusRemaining != nil || window.ResetAt.Format(time.RFC3339) != quotaTestReset {
		t.Fatalf("unexpected workspace metadata: %+v", window)
	}
	quotaAssertNumber(t, window.Limit, 100)
	quotaAssertNumber(t, window.Used, 25)
	quotaAssertNumber(t, window.Remaining, 75)
	if !reflect.DeepEqual(before, *c.account) {
		t.Fatal("quota observation mutated account state")
	}
}

func TestFetchQuotaWorkspaceZeroExhaustedAndSeparate(t *testing.T) {
	workspaces := "[" + quotaTestWorkspace("0", "0") + "," + quotaTestWorkspace("10", "15") + "," + quotaTestWorkspace("30.5", "4.25") + "]"
	snapshot, err := quotaTestClient(t, quotaTestResponse(workspaces, "null")).FetchQuota(context.Background())
	if err != nil || snapshot.Status != "ok" || len(snapshot.Windows) != 3 {
		t.Fatalf("workspace windows not preserved: %+v, %v", snapshot, err)
	}
	for i, expected := range []struct{ limit, used, remaining float64 }{{0, 0, 0}, {10, 15, 0}, {30.5, 4.25, 26.25}} {
		window := snapshot.Windows[i]
		quotaAssertNumber(t, window.Limit, expected.limit)
		quotaAssertNumber(t, window.Used, expected.used)
		quotaAssertNumber(t, window.Remaining, expected.remaining)
		if window.BonusRemaining != nil || window.Scope != "workspace_monthly" || window.Unit != "credits" {
			t.Fatal("workspace windows were relabeled or given invented bonuses")
		}
	}
}

func TestFetchQuotaMissingAndInvalidWorkspacePartial(t *testing.T) {
	for _, invalid := range []string{
		`{}`, `null`, `{"includedUsageInfo":null}`, `{"includedUsageInfo":[]}`,
		quotaTestWorkspace("null", "0"), quotaTestWorkspace(`"NaN"`, "1"),
		quotaTestWorkspace("1e400", "1"), quotaTestWorkspace("-1", "1"),
		quotaTestWorkspace("false", "1"), quotaTestWorkspace("5", `"3"`),
		`{"includedUsageInfo":{"includedUsageCredits":5,"usageCreditsUsedSinceLastRefresh":0,"nextRefreshTime":"invalid"}}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			snapshot, err := quotaTestClient(t, quotaTestResponse("["+quotaTestWorkspace("100", "0")+","+invalid+"]", "null")).FetchQuota(context.Background())
			if err != nil || snapshot.Status != "partial" || len(snapshot.Windows) != 2 {
				t.Fatalf("missing workspace was not partial: %+v, %v", snapshot, err)
			}
			quotaAssertNumber(t, snapshot.Windows[0].Remaining, 100)
			if snapshot.Windows[1].Limit == nil || snapshot.Windows[1].Used == nil {
				if snapshot.Windows[1].Remaining != nil {
					t.Fatal("unknown measurement became a remaining balance")
				}
			}
			if snapshot.Windows[1].BonusRemaining != nil {
				t.Fatal("unknown bonus became measured")
			}
		})
	}
}

func TestFetchQuotaPartialMeasurementsRemainUnknown(t *testing.T) {
	response := quotaTestResponse(`[{"includedUsageInfo":{"includedUsageCredits":null,"usageCreditsUsedSinceLastRefresh":0}}]`, "null")
	snapshot, err := quotaTestClient(t, response).FetchQuota(context.Background())
	if err != nil || snapshot.Status != "partial" || len(snapshot.Windows) != 1 {
		t.Fatalf("partial measurement rejected: %+v, %v", snapshot, err)
	}
	window := snapshot.Windows[0]
	quotaAssertNumber(t, window.Used, 0)
	if window.Limit != nil || window.Remaining != nil || !window.ResetAt.IsZero() {
		t.Fatal("unknown fields were inferred from a measured zero")
	}

	response = `{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[` + quotaTestWorkspace("10", "0") + `]}}}}`
	snapshot, err = quotaTestClient(t, response).FetchQuota(context.Background())
	if err != nil || snapshot.Status != "ok" || len(snapshot.Windows) != 1 {
		t.Fatalf("irrelevant absent legacy field degraded current quota: %+v, %v", snapshot, err)
	}

	response = quotaTestResponse("null", `{"isUnlimited":true}`)
	snapshot, err = quotaTestClient(t, response).FetchQuota(context.Background())
	if err != nil || snapshot.Status != "partial" || len(snapshot.Windows) != 1 {
		t.Fatalf("missing workspace observation was silently ignored: %+v, %v", snapshot, err)
	}
}

func TestFetchQuotaLegacyUnlimitedOnly(t *testing.T) {
	snapshot, err := quotaTestClient(t, quotaTestResponse("[]", `{"isUnlimited":true}`)).FetchQuota(context.Background())
	if err != nil || snapshot.Status != "ok" || len(snapshot.Windows) != 1 {
		t.Fatalf("unlimited legacy observation rejected: %+v, %v", snapshot, err)
	}
	window := snapshot.Windows[0]
	if window.Scope != "personal" || window.Unit != "credits" || !window.HasUnlimited || !window.Unlimited || window.Limit != nil || window.Used != nil || window.Remaining != nil || window.BonusRemaining != nil || !window.ResetAt.IsZero() {
		t.Fatalf("unlimited legacy window invented measurements: %+v", window)
	}
}

func TestFetchQuotaLegacySeparateAndOptional(t *testing.T) {
	valid := `{"isUnlimited":false,"requestLimit":20,"requestsUsedSinceLastRefresh":3,"nextRefreshTime":"` + quotaTestReset + `"}`
	for _, tc := range []struct {
		name, legacy, status string
		windows              int
	}{
		{"absent", "null", "ok", 1},
		{"valid", valid, "ok", 2},
		{"missing used", `{"isUnlimited":false,"requestLimit":20}`, "partial", 2},
		{"missing unlimited", `{"requestLimit":20,"requestsUsedSinceLastRefresh":3}`, "partial", 2},
		{"unlimited wrong type", `{"isUnlimited":"true"}`, "partial", 2},
		{"unlimited invalid measurement", `{"isUnlimited":true,"requestLimit":-1}`, "partial", 2},
		{"wrong shape", `[]`, "partial", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := quotaTestClient(t, quotaTestResponse("["+quotaTestWorkspace("100", "40")+"]", tc.legacy)).FetchQuota(context.Background())
			if err != nil || snapshot.Status != tc.status || len(snapshot.Windows) != tc.windows {
				t.Fatalf("unexpected legacy handling: %+v, %v", snapshot, err)
			}
			quotaAssertNumber(t, snapshot.Windows[0].Remaining, 60)
			if tc.name == "valid" {
				legacy := snapshot.Windows[1]
				if legacy.Scope != "personal" || legacy.Unit != "credits" || !legacy.HasUnlimited || legacy.Unlimited {
					t.Fatal("legacy metadata lost")
				}
				quotaAssertNumber(t, legacy.Remaining, 17)
			}
		})
	}
}

func TestFetchQuotaErrorsAreFixedAndNoFallback(t *testing.T) {
	for _, response := range []string{
		`{"errors":[{"message":"synthetic-private-data"}]}`,
		`{"data":{"user":{"__typename":"UserFacingError","error":{"message":"synthetic-private-data"}}}}`,
		`{"data":{"user":{"__typename":"Unknown"}}}`,
		quotaTestResponse("[]", "null"), quotaTestResponse("[{}]", "null"),
		quotaTestResponse("null", "null"), quotaTestResponse(`"invalid"`, "null"),
		quotaTestResponse("["+quotaTestWorkspace("null", "null")+"]", "null"),
		`{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"includedUsageInfo":{"includedUsageCredits":NaN}}]}}}}`,
		`{"errors":[{"message":"synthetic-private-data"}],"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[` + quotaTestWorkspace("100", "1") + `]}}}}`,
	} {
		var calls atomic.Int64
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = io.WriteString(w, response)
		}))
		snapshot, err := NewFromAccount(testAccount(), testConfig(s.URL)).FetchQuota(context.Background())
		s.Close()
		if !errors.Is(err, errResponse) || snapshot.Status != "error" || snapshot.Source != quotaSource || snapshot.AttemptedAt.IsZero() || !snapshot.SyncedAt.IsZero() || calls.Load() != 1 {
			t.Fatalf("error not rejected without fallback: %+v, %v, calls %d", snapshot, err, calls.Load())
		}
		if strings.Contains(err.Error(), "synthetic-private") || strings.Contains(err.Error(), s.URL) {
			t.Fatal("quota error leaked upstream data")
		}
	}
}

func TestFetchQuotaHTTPFailureRedacted(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		var calls atomic.Int64
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"message":"synthetic-private-data"}`)
		}))
		snapshot, err := NewFromAccount(testAccount(), testConfig(s.URL)).FetchQuota(context.Background())
		s.Close()
		if !errors.Is(err, errResponse) || snapshot.Status != "error" || calls.Load() != 1 {
			t.Fatalf("HTTP failure was not rejected: %+v, %v", snapshot, err)
		}
		if strings.Contains(err.Error(), "synthetic-private") || strings.Contains(err.Error(), s.URL) {
			t.Fatal("HTTP failure leaked upstream data")
		}
	}
}

func TestFetchQuotaCancellationAndDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snapshot, err := quotaTestClient(t, quotaTestResponse("[]", `{"isUnlimited":true}`)).FetchQuota(ctx)
	if !errors.Is(err, context.Canceled) || snapshot.Status != "error" || !snapshot.SyncedAt.IsZero() {
		t.Fatalf("cancellation lost: %+v, %v", snapshot, err)
	}

	c := NewFromAccount(testAccount(), testConfig("http://quota.invalid"))
	c.http = &http.Client{Transport: quotaDeadlineTransport{t: t}}
	_, err = c.FetchQuota(context.Background())
	if err != nil {
		t.Fatalf("deadline transport failed: %v", err)
	}
}

type quotaDeadlineTransport struct{ t *testing.T }

func (rt quotaDeadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	deadline, ok := r.Context().Deadline()
	if remaining := time.Until(deadline); !ok || remaining <= 0 || remaining > 20*time.Second {
		rt.t.Error("quota request has no bounded 20-second context")
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(quotaTestResponse("[]", `{"isUnlimited":true}`)))}, nil
}

func TestQuotaNumberInvalidIsUnknown(t *testing.T) {
	for _, raw := range []string{"null", `"NaN"`, `"Infinity"`, "1e400", "-1", "false", "{}", "[]"} {
		var number quotaNumber
		if err := json.Unmarshal([]byte(raw), &number); err != nil || number.value != nil {
			t.Fatalf("invalid measurement %s not unknown: %+v, %v", raw, number, err)
		}
	}
	for _, raw := range []string{"0", "-0", "0.0"} {
		var number quotaNumber
		if err := json.Unmarshal([]byte(raw), &number); err != nil {
			t.Fatal(err)
		}
		quotaAssertNumber(t, number.value, 0)
	}
}

// Compile-time contract: callers receive the store-owned snapshot, not a
// transport shape containing workspace identifiers or raw upstream errors.
var _ func(*Client, context.Context) (store.WarpQuotaSnapshot, error) = (*Client).FetchQuota
