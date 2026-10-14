package cline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// planClient stands up a control plane that answers /users/me/plan with the
// given status and body, so the tier logic is tested against the shapes the
// upstream actually returns: a plan row for a subscriber, and the no-history
// sentence for an account that never subscribed.
func planClient(t *testing.T, status int, body string, capture *http.Header) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			*capture = r.Header.Clone()
		}
		if r.URL.Path != "/users/me/plan" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	return &Client{
		apiBase: server.URL,
		control: server.Client(),
		stream:  server.Client(),
		creds:   Credentials{AccessToken: "access-1", ExpiresAt: time.Now().Add(time.Hour)},
	}
}

// TestPlanReadsASubscriberNamesThePlan proves a subscriber's row becomes the
// tier rather than being flattened to "free". The catalog cannot answer this:
// it publishes the free list to every account, subscriber included.
func TestPlanReadsASubscriberNamesThePlan(t *testing.T) {
	client := planClient(t, http.StatusOK, `{"data":{"plan":{"displayName":"Cline Pass (Monthly)"}},"success":true}`, nil)
	plan, err := client.FetchPlan(context.Background())
	testutil.NoError(t, err, "FetchPlan() error = %v")
	testutil.CheckFalse(t, !plan.Explicit, "Explicit = false for a plan row the upstream returned")
	testutil.CheckEqual(t, plan.Name, "Cline Pass (Monthly)")
}

func TestPlanReadsCurrentSubscription(t *testing.T) {
	for name, body := range map[string]string{
		"official envelope": `{"success":true,"data":{"subscriptionId":"subscription-1","plan":{"name":"cline-pass-monthly","displayName":"Cline Pass (Monthly)"}}}`,
		"canonical name":    `{"success":true,"data":{"plan":{"name":"Cline Pass (Monthly)"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := planClient(t, http.StatusOK, body, nil).FetchPlan(context.Background())
			testutil.NoError(t, err)
			testutil.CheckEqual(t, plan.Name, "Cline Pass (Monthly)")
			testutil.True(t, plan.Explicit)
		})
	}
}

func TestPlanSubscriptionPeriod(t *testing.T) {
	for _, fixture := range []struct{ end, want string }{
		{"2020-01-01T00:00:00Z", "free"},
		{"2060-01-01T00:00:00Z", "Cline Pass"},
	} {
		t.Run(fixture.end, func(t *testing.T) {
			body := `{"success":true,"data":{"plan":{"displayName":"Cline Pass"},"canceledAt":"2020-01-01T00:00:00Z","currentPeriodEnd":"` + fixture.end + `"}}`
			plan, err := planClient(t, http.StatusOK, body, nil).FetchPlan(context.Background())
			testutil.NoError(t, err)
			testutil.CheckEqual(t, plan.Name, fixture.want)
			testutil.True(t, plan.Explicit)
		})
	}
}

// TestPlanNoHistoryMeansFree pins the free verdict on the sentence the upstream
// actually sends. It is the answer observed for the live account in this
// deployment, and it is corroborated rather than assumed: a pass-tier model
// answered 403 ENTITLEMENT_ERROR for that account while every free-tier model
// answered 200.
func TestPlanNoHistoryMeansFree(t *testing.T) {
	client := planClient(t, http.StatusNotFound, `{"data":null,"error":"no plan history found for user","success":false}`, nil)
	plan, err := client.FetchPlan(context.Background())
	testutil.NoError(t, err, "FetchPlan() error = %v")
	testutil.CheckFalsef(t, !plan.Explicit || plan.Name != "free", "Plan = %+v, want {Name: free, Explicit: true}", plan)
}

// TestPlanNeverInventsATier is the guard the whole file exists for. An endpoint
// that is down, or that answers something unrecognised, must leave the tier
// unknown: defaulting it to "free" would label a subscriber's account as free
// and hide the fact that the gateway never actually asked.
func TestPlanNeverInventsATier(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"upstream down":     {http.StatusBadGateway, `{"error":"upstream down"}`},
		"unrecognised 404":  {http.StatusNotFound, `{"error":"Not Found","success":false}`},
		"200 without a row": {http.StatusOK, `{"data":{},"success":true}`},
		"unparseable body":  {http.StatusOK, `not json`},
		"flat data row":     {http.StatusOK, `{"success":true,"data":{"displayName":"Cline Pass"}}`},
		"unwrapped row":     {http.StatusOK, `{"plan":{"displayName":"Cline Pass"}}`},
		"flat root name":    {http.StatusOK, `{"success":true,"displayName":"Cline Pass"}`},
		"legacy plan name":  {http.StatusOK, `{"success":true,"data":{"planName":"Cline Pass"}}`},
		"failed envelope":   {http.StatusOK, `{"success":false,"data":{"plan":{"name":"Cline Pass"}}}`},
		"401 no history":    {http.StatusUnauthorized, `{"error":"no plan history found for user","success":false}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client := planClient(t, tc.status, tc.body, nil)
			plan, err := client.FetchPlan(context.Background())
			testutil.Error(t, err, "FetchPlan() = %+v, want an error so no tier is recorded")
			testutil.CheckFalsef(t, plan.Explicit || plan.Name != "", "Plan = %+v, want it empty", plan)
		})
	}
}

// TestPlanRefusedCredentialIsNotAFreeVerdict keeps the tier read from being
// mistaken for a credential verdict. A 401 means the token is gone and the
// operator has to re-authorize; recording "free" there would bury it.
func TestPlanRefusedCredentialIsNotAFreeVerdict(t *testing.T) {
	client := planClient(t, http.StatusUnauthorized, `{"error":"unauthorized"}`, nil)
	plan, err := client.FetchPlan(context.Background())
	testutil.Error(t, err, "FetchPlan() = %+v, want an error")
	testutil.CheckEqual(t, plan.Name, "")
}

// TestClineAccountCarriesThePlanTier pins the storage contract the console
// reads: the tier lives in its own field so "never probed" and "probed and
// free" stay distinguishable, and a round trip through the store keeps it.
func TestClineAccountCarriesThePlanTier(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "cline-plan:"})
	testutil.NoError(t, err, "store.New() error = %v")
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	// A new account has never been probed: the field must stay empty rather
	// than defaulting to a tier the console would then display as fact.
	acc := &store.Account{AccountType: "cline", Enabled: true, Weight: 1}
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")
	testutil.CheckEqual(t, acc.ClinePlan, "")
	stored, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.CheckEqual(t, stored.ClinePlan, "")

	// A probed free tier survives a write and a reload, so "probed and free"
	// is distinguishable from the empty value above.
	acc.ClinePlan = "free"
	testutil.NoError(t, s.UpdateAccount(ctx, acc), "UpdateAccount() error = %v")
	stored, err = s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.CheckEqual(t, stored.ClinePlan, "free")
}

// TestPlanRequestSendsTheProductIdentity proves the tier read goes out with the
// same identity block chat needs: without it the upstream answers 403 for a
// product-surface reason that has nothing to do with the credential.
func TestPlanRequestSendsTheProductIdentity(t *testing.T) {
	var seen http.Header
	client := planClient(t, http.StatusOK, `{"data":{"plan":{"displayName":"Cline Pass"}},"success":true}`, &seen)
	_, err := client.FetchPlan(context.Background())
	testutil.CheckNoError(t, err)
	for _, name := range []string{"X-CLIENT-TYPE", "X-CORE-VERSION", "User-Agent"} {
		testutil.CheckEqual(t, seen.Get(name), defaultClientHeaders[name])
	}
	got := seen.Get("Authorization")
	testutil.CheckFalsef(t, !strings.HasPrefix(got, "Bearer workos:"), "Authorization = %q, want the workos bearer prefix", got)
}
