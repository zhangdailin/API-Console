package store

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func usageBound(value float64) *float64 { return &value }

func TestModelUsageNormalizeValidatesBoundsAndPreservesZero(t *testing.T) {
	for _, test := range []struct {
		name      string
		low, high *float64
		valid     bool
	}{
		{"zero", usageBound(0), usageBound(0), true},
		{"range", usageBound(0.5), usageBound(2), true},
		{"missing minimum", nil, usageBound(2), false},
		{"missing maximum", usageBound(2), nil, false},
		{"negative", usageBound(-1), usageBound(2), false},
		{"reversed", usageBound(2), usageBound(1), false},
		{"nan", usageBound(math.NaN()), usageBound(1), false},
		{"infinity", usageBound(1), usageBound(math.Inf(1)), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := &ModelUsageMetadata{CreditMultiplierMin: test.low, CreditMultiplierMax: test.high, AccountCount: 1, Source: " WARP_FEATURE_MODEL_CHOICE ", ObservedAt: time.Now()}
			got := input.Normalize()
			if got == nil || (got.CreditMultiplierMin != nil) != test.valid || (got.CreditMultiplierMax != nil) != test.valid {
				t.Fatalf("normalized bounds = %+v; valid = %v", got, test.valid)
			}
			if got.Source != "warp_feature_model_choice" || got.ObservedAt.Location() != time.UTC {
				t.Fatalf("source/time not normalized: %+v", got)
			}
			if test.valid {
				if got.AccountCount != 1 || *got.CreditMultiplierMin != *test.low || got.Conflicting != (*test.low != *test.high) {
					t.Fatalf("valid observation changed: %+v", got)
				}
			} else if got.AccountCount != 0 || got.MissingAccountCount != 1 {
				t.Fatalf("invalid observation counted as observed: %+v", got)
			}
			if _, err := json.Marshal(got); err != nil {
				t.Fatalf("normalized metadata cannot be serialized: %v", err)
			}
		})
	}
}

func TestModelUsageCloneAndNormalizationDoNotAlias(t *testing.T) {
	original := &Model{Capabilities: []string{"chat"}, BoundAccountIDs: []int64{1}, UpstreamUsage: &ModelUsageMetadata{
		CreditMultiplierMin: usageBound(0), CreditMultiplierMax: usageBound(2),
		RequestMultiplierMin: usageBound(1), RequestMultiplierMax: usageBound(3), AccountCount: 1, Source: "upstream",
	}}
	clone := original.Clone()
	clone.Capabilities[0], clone.BoundAccountIDs[0] = "image", 2
	*clone.UpstreamUsage.CreditMultiplierMin, *clone.UpstreamUsage.CreditMultiplierMax = 9, 9
	*clone.UpstreamUsage.RequestMultiplierMin, *clone.UpstreamUsage.RequestMultiplierMax = 9, 9
	if original.Capabilities[0] != "chat" || original.BoundAccountIDs[0] != 1 || *original.UpstreamUsage.CreditMultiplierMin != 0 || *original.UpstreamUsage.CreditMultiplierMax != 2 || *original.UpstreamUsage.RequestMultiplierMin != 1 || *original.UpstreamUsage.RequestMultiplierMax != 3 {
		t.Fatal("model clone aliases original")
	}
	before := original.UpstreamUsage
	original.NormalizeRoute()
	*original.UpstreamUsage.CreditMultiplierMin = 4
	if *before.CreditMultiplierMin != 0 {
		t.Fatal("normalization aliases original metadata")
	}
	if (*Model)(nil).Clone() != nil || (*ModelUsageMetadata)(nil).Clone() != nil || (*ModelUsageMetadata)(nil).Normalize() != nil {
		t.Fatal("nil cloning/normalization must remain nil")
	}
}

func TestModelUsageJSONPreservesExplicitZeroAndOmitsAbsent(t *testing.T) {
	row := &Model{UpstreamUsage: &ModelUsageMetadata{CreditMultiplierMin: usageBound(0), CreditMultiplierMax: usageBound(0), AccountCount: 1, Source: "upstream"}}
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(fields["upstream_usage"], &usage); err != nil {
		t.Fatal(err)
	}
	if string(usage["credit_multiplier_min"]) != "0" || string(usage["credit_multiplier_max"]) != "0" {
		t.Fatalf("explicit zero lost: %s", data)
	}
	if _, exists := usage["request_multiplier_min"]; exists {
		t.Fatal("unknown request multiplier serialized")
	}
	if err := json.Unmarshal(data, row); err != nil {
		t.Fatal(err)
	}
	row.UpstreamUsage = nil
	data, err = json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	// Decode into a fresh map because Unmarshal preserves existing map entries.
	fields = nil
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["upstream_usage"]; exists {
		t.Fatal("absent usage serialized")
	}
}

func TestModelUsageReconciliationUpdatesAndClearsObservation(t *testing.T) {
	for _, origin := range []string{"manual", "discovery"} {
		t.Run(origin, func(t *testing.T) {
			s, _ := newTestRedisStore(t, "usage:")
			ctx := t.Context()
			row := &Model{Channel: "Cline", ModelID: "shared", Name: "Original", Status: ModelStatusMaintenance, Origin: origin,
				UpstreamUsage: &ModelUsageMetadata{CreditMultiplierMin: usageBound(1), CreditMultiplierMax: usageBound(1), Source: "upstream", AccountCount: 1}}
			if err := s.CreateModel(ctx, row); err != nil {
				t.Fatal(err)
			}
			observed := &Model{ModelID: row.ModelID, Status: ModelStatusAvailable, UpstreamUsage: &ModelUsageMetadata{
				CreditMultiplierMin: usageBound(0), CreditMultiplierMax: usageBound(2), Source: "upstream", AccountCount: 2,
			}}
			result, err := s.ReconcileDiscoveredModels(ctx, "Cline", []*Model{observed}, ModelReconcileOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if result.Updated != 1 {
				t.Fatalf("usage-only update not reported: %+v", result)
			}
			got, err := s.GetModel(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.UpstreamUsage == nil || *got.UpstreamUsage.CreditMultiplierMin != 0 || *got.UpstreamUsage.CreditMultiplierMax != 2 || !got.UpstreamUsage.Conflicting {
				t.Fatalf("usage update lost: %+v", got.UpstreamUsage)
			}
			// Admin presentation/status updates retain discovery-owned evidence.
			got.Status = ModelStatusOffline
			if err := s.UpdateModel(ctx, got); err != nil {
				t.Fatal(err)
			}
			got, err = s.GetModel(ctx, row.ID)
			if err != nil || got.UpstreamUsage == nil {
				t.Fatalf("status edit lost usage: %+v %v", got, err)
			}
			observed.UpstreamUsage = nil
			if _, err := s.ReconcileDiscoveredModels(ctx, "Cline", []*Model{observed}, ModelReconcileOptions{Prune: true}); err != nil {
				t.Fatal(err)
			}
			got, err = s.GetModel(ctx, row.ID)
			if err != nil || got.UpstreamUsage != nil {
				t.Fatalf("latest absent usage retained stale values: %+v %v", got, err)
			}
		})
	}
}
