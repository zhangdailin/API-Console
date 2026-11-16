package modelrefresh

import (
	"reflect"
	"testing"
	"time"

	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

func modelUsageValue(value float64) *float64 { return &value }

func modelUsageObservation(credit, request float64, at time.Time) *store.ModelUsageMetadata {
	return &store.ModelUsageMetadata{
		CreditMultiplierMin: modelUsageValue(credit), CreditMultiplierMax: modelUsageValue(credit),
		RequestMultiplierMin: modelUsageValue(request), RequestMultiplierMax: modelUsageValue(request),
		AccountCount: 1, Source: "warp_feature_model_choice", ObservedAt: at,
	}
}

func TestUnionAccountUsageAggregatesRangesAndMissingCounterparts(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	first := modelUsageObservation(0, 1, at)
	second := modelUsageObservation(3, 2, at.Add(time.Hour))
	attempts := []accountModelDiscoveryAttempt{
		{AccountID: 1, Candidates: []discoveredModel{{ID: "shared", UpstreamUsage: first}, {ID: "shared", UpstreamUsage: first}, {ID: "first-only", UpstreamUsage: first}}},
		{AccountID: 2, Candidates: []discoveredModel{{ID: "SHARED", UpstreamUsage: second}}},
		{AccountID: 3, Candidates: []discoveredModel{{ID: " shared "}}},
		{AccountID: 1, Candidates: []discoveredModel{{ID: "shared", UpstreamUsage: first}}},
	}
	got := unionAccountCatalogAttempts(attempts)
	if len(got) != 2 {
		t.Fatalf("got %d union rows", len(got))
	}
	usage := got[0].UpstreamUsage
	if usage == nil || usage.AccountCount != 2 || usage.MissingAccountCount != 1 || !usage.Conflicting || *usage.CreditMultiplierMin != 0 || *usage.CreditMultiplierMax != 3 || *usage.RequestMultiplierMin != 1 || *usage.RequestMultiplierMax != 2 || !usage.ObservedAt.Equal(at) {
		t.Fatalf("wrong aggregate: %+v", usage)
	}
	if single := got[1].UpstreamUsage; single.AccountCount != 1 || single.MissingAccountCount != 0 || single.Conflicting {
		t.Fatalf("nonshared model counted other accounts: %+v", single)
	}
	*usage.CreditMultiplierMin = 9
	if *first.CreditMultiplierMin != 0 {
		t.Fatal("union metadata aliases account observation")
	}
}

func TestUnionAccountUsageUniformUnknownAndMixedSources(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	one := modelUsageObservation(0, 2, at)
	otherSource := one.Clone()
	otherSource.Source = "different_source"
	partial := one.Clone()
	partial.RequestMultiplierMin, partial.RequestMultiplierMax = nil, nil
	for _, test := range []struct {
		name                                string
		observations                        []*store.ModelUsageMetadata
		count, missing                      int
		conflicting, absent, requestUnknown bool
	}{
		{name: "uniform explicit zero", observations: []*store.ModelUsageMetadata{one, one}, count: 2},
		{name: "unknown counterpart", observations: []*store.ModelUsageMetadata{nil, one}, count: 1, missing: 1},
		{name: "all unknown", observations: []*store.ModelUsageMetadata{nil, nil}, absent: true},
		{name: "different source", observations: []*store.ModelUsageMetadata{one, otherSource}, missing: 2, conflicting: true, requestUnknown: true},
		{name: "partial metric", observations: []*store.ModelUsageMetadata{one, partial}, count: 2, requestUnknown: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := aggregateAccountUsage(test.observations)
			if test.absent {
				if got != nil {
					t.Fatalf("all unknown = %+v", got)
				}
				return
			}
			if got == nil || got.AccountCount != test.count || got.MissingAccountCount != test.missing || got.Conflicting != test.conflicting || (got.RequestMultiplierMin == nil) != test.requestUnknown {
				t.Fatalf("aggregate = %+v", got)
			}
			if test.name == "different source" && (got.Source != "" || got.CreditMultiplierMin != nil) {
				t.Fatalf("mixed source selected first value: %+v", got)
			}
			reversed := make([]*store.ModelUsageMetadata, len(test.observations))
			for i := range test.observations {
				reversed[len(reversed)-1-i] = test.observations[i]
			}
			if !reflect.DeepEqual(got, aggregateAccountUsage(reversed)) {
				t.Fatal("aggregate depends on account order")
			}
		})
	}
}

func TestToDiscoveredUsageIsIndependent(t *testing.T) {
	usage := modelUsageObservation(0.5, 2, time.Now())
	rows := toDiscovered([]provider.Model{{ID: "model", UpstreamUsage: usage}})
	if len(rows) != 1 || rows[0].UpstreamUsage == nil || *rows[0].UpstreamUsage.CreditMultiplierMin != 0.5 {
		t.Fatalf("usage lost: %+v", rows)
	}
	*rows[0].UpstreamUsage.CreditMultiplierMin = 9
	if *usage.CreditMultiplierMin != 0.5 {
		t.Fatal("discovered model aliases provider observation")
	}
}

func TestApplyModelRefreshUsageChangesAndClearsWithoutBillingEffects(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := t.Context()
	clearModelsForChannel(t, ctx, s, "Warp")
	candidate := discoveredModel{ID: "model", Name: "Model", Verified: true, UpstreamUsage: modelUsageObservation(0, 0, time.Now())}
	if _, err := applyModelRefreshWithPrune(ctx, s, "Warp", "warp_feature_model_choice", []discoveredModel{candidate}, true); err != nil {
		t.Fatal(err)
	}
	row, err := s.GetModelByChannelAndModelID(ctx, "Warp", "model")
	if err != nil {
		t.Fatal(err)
	}
	if row.UpstreamUsage == nil || *row.UpstreamUsage.CreditMultiplierMin != 0 || row.BillingTier != "" || row.BillingSource != "" {
		t.Fatalf("usage lost or changed billing: %+v", row)
	}
	candidate.UpstreamUsage = modelUsageObservation(2, 3, time.Now())
	result, err := applyModelRefreshWithPrune(ctx, s, "Warp", "warp_feature_model_choice", []discoveredModel{candidate}, true)
	if err != nil || result.Updated != 1 {
		t.Fatalf("metadata-only update ignored: %+v %v", result, err)
	}
	row, err = s.GetModelByChannelAndModelID(ctx, "Warp", "model")
	if err != nil || row.UpstreamUsage == nil || *row.UpstreamUsage.CreditMultiplierMin != 2 {
		t.Fatalf("metadata update lost: %+v %v", row, err)
	}
	candidate.UpstreamUsage = nil
	if _, err := applyModelRefreshWithPrune(ctx, s, "Warp", "warp_feature_model_choice", []discoveredModel{candidate}, true); err != nil {
		t.Fatal(err)
	}
	row, err = s.GetModelByChannelAndModelID(ctx, "Warp", "model")
	if err != nil || row.UpstreamUsage != nil {
		t.Fatalf("absent latest evidence retained: %+v %v", row, err)
	}
}
