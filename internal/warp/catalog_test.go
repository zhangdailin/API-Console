package warp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/store"
)

const allPlanesCatalogResponse = `{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"featureModelChoice":{
	"agentMode":{"defaultId":"auto","choices":[
		{"id":"shared-model","disableReason":null,"usageMetadata":{"creditMultiplier":1},"contextWindow":{"default":200000,"max":300000,"min":1000,"isConfigurable":true}},
		{"id":"auto","disableReason":null},
		{"id":"model-disabled","disableReason":"not available"},
		{"id":"bad model","disableReason":null}
	]},
	"planning":{"defaultId":"auto","choices":[
		{"id":"shared-model","disableReason":null,"usageMetadata":{"creditMultiplier":1},"contextWindow":{"default":200000,"max":300000,"min":1000,"isConfigurable":true}},
		{"id":"planning-model","disableReason":"","contextWindow":{"default":64000,"max":128000}}
	]},
	"coding":{"defaultId":"coding","choices":[
		{"id":"coding-model","contextWindow":{"default":0,"max":32000}},
		{"id":"all","disableReason":null}
	]},
	"cliAgent":{"defaultId":"cli-agent","choices":[
		{"id":"cli-agent","contextWindow":{"default":1000}},
		{"id":"agent-cli-exec","contextWindow":{"default":16000,"max":32000}}
	]},
	"computerUseAgent":{"defaultId":"computer-use-agent","choices":[
		{"id":"computer-use-agent","contextWindow":{"default":1000}},
		{"id":"agent-computer-model","contextWindow":{"max":8000}}
	]}
}}]}}}}`

func planesCatalogClient(t *testing.T, response string) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql/v2" || r.URL.Query().Get("op") != "GetFeatureModelChoices" {
			t.Error("unexpected catalog request")
			http.NotFound(w, r)
			return
		}
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode catalog request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		for _, field := range []string{
			"agentMode", "planning", "coding", "cliAgent", "computerUseAgent",
			"contextWindow { default max min isConfigurable }",
		} {
			if !strings.Contains(request.Query, field) {
				t.Errorf("catalog query does not request %q", field)
			}
		}
		// contextWindow is a direct LlmInfo field. Nesting it under spec is a
		// GraphQL error and would fail every catalog read on a live account.
		if strings.Contains(request.Query, "spec") {
			t.Error("catalog query must not nest contextWindow under spec")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return NewFromAccount(testAccount(), testConfig(server.URL))
}

func catalogResponseWithChoices(choices string) string {
	return `{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"featureModelChoice":{"agentMode":{"defaultId":"auto","choices":[` + choices + `]}}}]}}}}`
}

func TestFetchModelChoicesReadsAllPlanesAndDedupes(t *testing.T) {
	client := planesCatalogClient(t, allPlanesCatalogResponse)
	choices, err := client.FetchModelChoices(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelChoice{
		{ID: "agent-cli-exec", ContextWindow: 16000},
		{ID: "agent-computer-model", ContextWindow: 8000},
		{ID: "coding-model", ContextWindow: 32000},
		{ID: "planning-model", ContextWindow: 64000, CreditMultiplier: nil},
		{ID: "shared-model", ContextWindow: 200000, CreditMultiplier: floatPtr(1)},
	}
	gotIDs := make([]string, 0, len(choices))
	for _, choice := range choices {
		gotIDs = append(gotIDs, choice.ID)
	}
	wantIDs := make([]string, 0, len(want))
	for _, choice := range want {
		wantIDs = append(wantIDs, choice.ID)
	}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("published union changed: got %v, want %v", gotIDs, wantIDs)
	}
	for i, choice := range choices {
		if choice.ContextWindow != want[i].ContextWindow {
			t.Errorf("%s: window = %d, want %d", choice.ID, choice.ContextWindow, want[i].ContextWindow)
		}
		assertCatalogMultiplier(t, choice.ID+" credit", choice.CreditMultiplier, want[i].CreditMultiplier)
	}
	// The shared model appears in two planes but is published once, and CLI /
	// computer-use IDs remain excluded while the rest of those planes count.
	if slices.Contains(gotIDs, "cli-agent") || slices.Contains(gotIDs, "computer-use-agent") ||
		slices.Contains(gotIDs, "auto") || slices.Contains(gotIDs, "all") ||
		slices.Contains(gotIDs, "model-disabled") || slices.Contains(gotIDs, "bad model") {
		t.Fatalf("excluded catalog identity was published: %v", gotIDs)
	}
	ids, err := client.FetchModels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, gotIDs) {
		t.Fatalf("FetchModels and FetchModelChoices disagree: %v vs %v", ids, gotIDs)
	}
}

func floatPtr(value float64) *float64 { return &value }

func TestFetchModelChoicesContextWindowSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec string
		want int
	}{
		{name: "default_positive", spec: `,"contextWindow":{"default":1000,"max":2000}`, want: 1000},
		{name: "default_zero_uses_max", spec: `,"contextWindow":{"default":0,"max":2000}`, want: 2000},
		{name: "default_negative_uses_max", spec: `,"contextWindow":{"default":-5,"max":2000}`, want: 2000},
		{name: "default_null_uses_max", spec: `,"contextWindow":{"default":null,"max":2000}`, want: 2000},
		{name: "default_absent_uses_max", spec: `,"contextWindow":{"max":2000}`, want: 2000},
		{name: "fractional_default_uses_max", spec: `,"contextWindow":{"default":1.5,"max":2000}`, want: 2000},
		{name: "string_default_uses_max", spec: `,"contextWindow":{"default":"1000","max":2000}`, want: 2000},
		{name: "overflow_default_uses_max", spec: `,"contextWindow":{"default":1e999,"max":2000}`, want: 2000},
		{name: "max_only_negative", spec: `,"contextWindow":{"max":-1}`, want: 0},
		{name: "both_missing", spec: `,"contextWindow":{}`, want: 0},
		{name: "both_null", spec: `,"contextWindow":{"default":null,"max":null}`, want: 0},
		{name: "min_is_not_a_window", spec: `,"contextWindow":{"default":0,"max":0,"min":128000,"isConfigurable":true}`, want: 0},
		{name: "spec_absent", spec: ``, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := planesCatalogClient(t, catalogResponseWithChoices(`{"id":"window-model","disableReason":null`+tc.spec+`}`))
			choices, err := client.FetchModelChoices(t.Context())
			if err != nil || len(choices) != 1 {
				t.Fatalf("window metadata rejected an active model: %v", err)
			}
			if choices[0].ContextWindow != tc.want {
				t.Fatalf("window = %d, want %d", choices[0].ContextWindow, tc.want)
			}
		})
	}
}

func TestCatalogObservationRecordsAndClearsWindows(t *testing.T) {
	client := planesCatalogClient(t, allPlanesCatalogResponse)
	observation, err := client.observeCatalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	acc := testAccount()
	acc.WarpModelWindows = map[string]int{"stale-model": 1}
	if !observation.Record(acc, time.Now()) {
		t.Fatal("successful observation was not recorded")
	}
	want := map[string]int{
		"agent-cli-exec": 16000, "agent-computer-model": 8000, "coding-model": 32000,
		"planning-model": 64000, "shared-model": 200000,
	}
	if !reflect.DeepEqual(acc.WarpModelWindows, want) {
		t.Fatalf("recorded windows = %v, want %v", acc.WarpModelWindows, want)
	}

	withoutWindows := planesCatalogClient(t, catalogResponseWithChoices(`{"id":"observed-model","disableReason":null}`))
	emptyObservation, err := withoutWindows.observeCatalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	acc = testAccount()
	acc.WarpModelWindows = map[string]int{"stale-model": 1}
	if !emptyObservation.Record(acc, time.Now()) || acc.WarpModelWindows != nil {
		t.Fatal("an observation without contextWindow evidence inherited a stale window")
	}
}

func TestCapabilitiesContextWindowsObservedOnly(t *testing.T) {
	var caps capabilities
	if input, output := caps.ContextWindows(nil); input != nil || output != nil {
		t.Fatalf("nil account produced budgets: %v %v", input, output)
	}
	acc := &store.Account{WarpModelWindows: map[string]int{
		"model-a": 200000, "model-b": 0, "auto": 1000, "cli-agent": 5000,
	}}
	input, output := caps.ContextWindows(acc)
	if !reflect.DeepEqual(input, map[string]int{"model-a": 200000}) {
		t.Fatalf("context windows = %v", input)
	}
	if output != nil {
		t.Fatalf("warp declares no output budget, got %v", output)
	}
	if _, ok := input["model-missing"]; ok {
		t.Fatal("a window was invented for a model that was never observed")
	}
	input["model-a"] = 1
	if acc.WarpModelWindows["model-a"] != 200000 {
		t.Fatal("capability read exposed the account's mutable window map")
	}
	if input, output := caps.ContextWindows(&store.Account{}); input != nil || output != nil {
		t.Fatalf("empty observation produced budgets: %v %v", input, output)
	}
}
