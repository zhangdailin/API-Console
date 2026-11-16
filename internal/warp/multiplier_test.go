package warp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/provider"
)

func multiplierCatalogClient(t *testing.T, workspaces string) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql/v2" || r.URL.Query().Get("op") != "GetFeatureModelChoices" {
			t.Error("unexpected catalog request")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !strings.Contains(request.Query, "usageMetadata { creditMultiplier requestMultiplier }") {
			t.Error("catalog query did not request both observed multiplier fields")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[%s]}}}}`, workspaces)
	}))
	t.Cleanup(server.Close)
	return NewFromAccount(testAccount(), testConfig(server.URL))
}

func multiplierWorkspace(choices string) string {
	return `{"featureModelChoice":{"agentMode":{"choices":[` + choices + `]}}}`
}

func assertCatalogMultiplier(t *testing.T, label string, got, want *float64) {
	t.Helper()
	if !equalCatalogMultiplier(got, want) {
		t.Errorf("%s: got %v, want %v", label, got, want)
	}
}

func TestCatalogMultiplierFreshObservation(t *testing.T) {
	value := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name    string
		usage   string
		credit  *float64
		request *float64
	}{
		{name: "missing"},
		{name: "null", usage: `null`},
		{name: "empty", usage: `{}`},
		{name: "null_fields", usage: `{"creditMultiplier":null,"requestMultiplier":null}`},
		{name: "zero", usage: `{"creditMultiplier":0,"requestMultiplier":0}`, credit: value(0), request: value(0)},
		{name: "fractional", usage: `{"creditMultiplier":0.125,"requestMultiplier":2.5}`, credit: value(0.125), request: value(2.5)},
		{name: "credit_only", usage: `{"creditMultiplier":1.25}`, credit: value(1.25)},
		{name: "legacy_request_only", usage: `{"requestMultiplier":3}`, request: value(3)},
		{name: "exponent", usage: `{"creditMultiplier":1e-3,"requestMultiplier":1e2}`, credit: value(0.001), request: value(100)},
		{name: "negative", usage: `{"creditMultiplier":-0.1,"requestMultiplier":-1}`},
		{name: "numeric_strings", usage: `{"creditMultiplier":"1.5","requestMultiplier":"0"}`},
		{name: "nonfinite_strings", usage: `{"creditMultiplier":"NaN","requestMultiplier":"Infinity"}`},
		{name: "overflow", usage: `{"creditMultiplier":1e999,"requestMultiplier":-1e999}`},
		{name: "invalid_field_shapes", usage: `{"creditMultiplier":{},"requestMultiplier":[1]}`},
		{name: "boolean_fields", usage: `{"creditMultiplier":true,"requestMultiplier":false}`},
		{name: "one_invalid_field", usage: `{"creditMultiplier":1e999,"requestMultiplier":0.5}`, request: value(0.5)},
		{name: "scalar_metadata", usage: `3`},
		{name: "string_metadata", usage: `"bad"`},
		{name: "array_metadata", usage: `[]`},
		{name: "boolean_metadata", usage: `false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			choice := `{"id":"observed-model","disableReason":null`
			if tc.usage != "" {
				choice += `,"usageMetadata":` + tc.usage
			}
			client := multiplierCatalogClient(t, multiplierWorkspace(choice+`}`))
			before := time.Now().UTC()
			observation, err := client.observeCatalog(t.Context())
			after := time.Now().UTC()
			if err != nil || len(observation.Models) != 1 || observation.Models[0].ID != "observed-model" {
				t.Fatalf("invalid usage metadata rejected an otherwise active model: %v", err)
			}
			model := observation.Models[0]
			if model.BillingTier != "" || model.BillingSource != "" {
				t.Fatal("usage metadata was converted into billing classification")
			}
			usage := model.UpstreamUsage
			if tc.credit == nil && tc.request == nil {
				if usage != nil {
					t.Fatalf("unknown multipliers produced usage metadata: %+v", usage)
				}
				return
			}
			if usage == nil {
				t.Fatal("known upstream multiplier was lost")
			}
			assertCatalogMultiplier(t, "credit minimum", usage.CreditMultiplierMin, tc.credit)
			assertCatalogMultiplier(t, "credit maximum", usage.CreditMultiplierMax, tc.credit)
			assertCatalogMultiplier(t, "request minimum", usage.RequestMultiplierMin, tc.request)
			assertCatalogMultiplier(t, "request maximum", usage.RequestMultiplierMax, tc.request)
			if usage.AccountCount != 1 || usage.MissingAccountCount != 0 || usage.Conflicting || usage.Source != "warp_feature_model_choice" || usage.ObservedAt.Before(before) || usage.ObservedAt.After(after) || usage.ObservedAt.Location() != time.UTC {
				t.Fatalf("incorrect per-account observation provenance: %+v", usage)
			}
		})
	}
}

func TestCatalogMultiplierDuplicateAccountChoices(t *testing.T) {
	for _, tc := range []struct {
		name       string
		choices    []string
		known      bool
		wantCredit float64
	}{
		{name: "identical", choices: []string{`{"creditMultiplier":1.5}`, `{"creditMultiplier":1.5}`}, known: true, wantCredit: 1.5},
		{name: "identical_zero", choices: []string{`{"creditMultiplier":0}`, `{"creditMultiplier":0}`}, known: true},
		{name: "credit_conflict_stays_unknown", choices: []string{`{"creditMultiplier":1}`, `{"creditMultiplier":2}`, `{"creditMultiplier":1}`}},
		{name: "request_conflict_invalidates_account", choices: []string{`{"creditMultiplier":1,"requestMultiplier":1}`, `{"creditMultiplier":1,"requestMultiplier":2}`}},
		{name: "known_then_unknown", choices: []string{`{"creditMultiplier":1}`, `null`}},
		{name: "unknown_then_known", choices: []string{`null`, `{"creditMultiplier":1}`}},
		{name: "invalid_then_known", choices: []string{`{"creditMultiplier":-1}`, `{"creditMultiplier":1}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var workspaces []string
			for _, usage := range tc.choices {
				workspaces = append(workspaces, multiplierWorkspace(`{"id":"duplicate-model","usageMetadata":`+usage+`}`))
			}
			client := multiplierCatalogClient(t, strings.Join(workspaces, ","))
			observation, err := client.observeCatalog(t.Context())
			if err != nil || len(observation.Models) != 1 {
				t.Fatalf("account duplicate did not retain exactly one active ID: %v", err)
			}
			usage := observation.Models[0].UpstreamUsage
			if !tc.known {
				if usage != nil {
					t.Fatalf("conflicting account choices produced a first-wins value or range: %+v", usage)
				}
				return
			}
			if usage == nil || usage.AccountCount != 1 || usage.Conflicting {
				t.Fatalf("identical duplicate counted as multiple observations: %+v", usage)
			}
			assertCatalogMultiplier(t, "credit minimum", usage.CreditMultiplierMin, &tc.wantCredit)
			assertCatalogMultiplier(t, "credit maximum", usage.CreditMultiplierMax, &tc.wantCredit)
		})
	}
}

func TestCatalogMultiplierActiveFilteringAndSnapshotIsolation(t *testing.T) {
	client := multiplierCatalogClient(t, multiplierWorkspace(`
		{"id":"z-model","usageMetadata":{"creditMultiplier":0.75}},
		{"id":"a-model","disableReason":"","usageMetadata":{"creditMultiplier":0}},
		{"id":"a-model","disableReason":"upgrade","usageMetadata":{"creditMultiplier":100}},
		{"id":"disabled-model","disableReason":{"reason":"disabled"},"usageMetadata":{"creditMultiplier":1}},
		{"id":"auto","usageMetadata":{"creditMultiplier":1}},
		{"id":"vendor/auto","usageMetadata":{"creditMultiplier":1}},
		{"id":"all","usageMetadata":{"creditMultiplier":1}},
		{"id":"cli-agent","usageMetadata":{"creditMultiplier":1}},
		{"id":"computer-use-agent","usageMetadata":{"creditMultiplier":1}},
		{"id":"invalid id","usageMetadata":{"creditMultiplier":1}}
	`))
	ids, err := client.FetchModels(t.Context())
	if err != nil || !slices.Equal(ids, []string{"a-model", "z-model"}) {
		t.Fatalf("FetchModels compatibility or active filtering changed: %v, %v", ids, err)
	}
	acc := testAccount()
	original := acc.Clone()
	observation, err := catalogPort().Observe(t.Context(), provider.Env{Config: testConfig(client.base)}, acc)
	if err != nil || len(observation.Models) != 2 {
		t.Fatalf("fresh observation failed: %v", err)
	}
	zero := 0.0
	if observation.Models[0].UpstreamUsage == nil {
		t.Fatal("active zero multiplier was lost")
	}
	assertCatalogMultiplier(t, "disabled duplicate must not override active zero", observation.Models[0].UpstreamUsage.CreditMultiplierMin, &zero)
	if !reflect.DeepEqual(acc, original) {
		t.Fatal("catalog observation changed the caller's account")
	}
	now := time.Now().UTC()
	if !observation.Record(acc, now) || !slices.Equal(acc.WarpModelIDs, ids) || !acc.WarpModelsSyncedAt.Equal(now) {
		t.Fatal("catalog no longer records the exact ID-only account snapshot")
	}
	original.WarpModelIDs = slices.Clone(acc.WarpModelIDs)
	original.WarpModelsSyncedAt = now
	if !reflect.DeepEqual(acc, original) {
		t.Fatal("usage recording changed non-catalog account fields")
	}
	for _, model := range catalogModels(acc.WarpModelIDs) {
		if model.UpstreamUsage != nil {
			t.Fatal("usage metadata was reconstructed from cached model IDs")
		}
	}
	if models := catalogPort().LastKnown(acc); len(models) != 0 {
		t.Fatal("cached account snapshot was published as a fresh usage observation")
	}
}

func TestCatalogMultiplierFailedReadHasNoCachedMetadata(t *testing.T) {
	client := multiplierCatalogClient(t, multiplierWorkspace(`{"id":"auto","usageMetadata":{"creditMultiplier":1}}`))
	observation, err := client.observeCatalog(t.Context())
	if err == nil || len(observation.Models) != 0 || observation.Record != nil {
		t.Fatal("failed active catalog read returned a cached observation")
	}
}
