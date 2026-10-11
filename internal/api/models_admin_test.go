package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func seedAdminModels(t *testing.T, s *store.Store, models ...*store.Model) {
	t.Helper()
	for _, m := range models {
		err := s.CreateModel(context.Background(), m)
		testutil.CheckNoError(t, err)
		if m.Channel == "grok" {
			testutil.CheckNoError(t, s.CreateAccount(context.Background(), &store.Account{AccountType: "grok", Enabled: true, GrokModels: []string{m.ModelID}}))
		}
	}
}

// Without a page parameter the endpoint keeps returning the bare array the
// bundled admin UI decodes.
func TestHandleModelsStaysABareArrayWithoutPaging(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	seedAdminModels(t, s, &store.Model{ID: "grok-4.6", Channel: "grok", ModelID: "grok-4.6", Name: "Grok 4.6"})

	rec := httptest.NewRecorder()
	a.HandleModels(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))

	testutil.Equal(t, rec.Code, http.StatusOK)
	var bare []store.Model
	err := json.Unmarshal(rec.Body.Bytes(), &bare)
	testutil.CheckNoError(t, err)
	testutil.Equal(t, len(bare), 1)
	testutil.Equal(t, bare[0].ModelID, "grok-4.6")
}

// Asking for a page switches to the optional paged envelope.
func TestHandleModelsServesPagedEnvelopeOnRequest(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	testutil.CheckNoError(t, s.CreateAccount(context.Background(), &store.Account{AccountType: "workbuddy", Enabled: true, UsageLimit: 100, UsageCurrent: 10}))
	seedAdminModels(t, s,
		&store.Model{ID: "grok-4.6", Channel: "grok", ModelID: "grok-4.6", Name: "Grok 4.6"},
		&store.Model{ID: "grok-4.5", Channel: "grok", ModelID: "grok-4.5", Name: "Grok 4.5"},
		&store.Model{ID: "wb-claude", Channel: "workbuddy", ModelID: "claude", Name: "Claude"},
	)

	rec := httptest.NewRecorder()
	a.HandleModels(rec, httptest.NewRequest(http.MethodGet, "/api/models?page=1&pageSize=2", nil))

	var envelope adminModelListEnvelope
	err := json.Unmarshal(rec.Body.Bytes(), &envelope)
	testutil.CheckNoError(t, err)
	testutil.Falsef(t, envelope.Total != 3 || envelope.Page != 1 || envelope.PageSize != 2 || len(envelope.Items) != 2, "envelope=%+v", envelope)

	rec = httptest.NewRecorder()
	a.HandleModels(rec, httptest.NewRequest(http.MethodGet, "/api/models?page=2&pageSize=2&search=grok", nil))
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope), "filtered envelope decode failed: %v")
	testutil.Equal(t, envelope.Total, 2)
	testutil.Equal(t, len(envelope.Items), 0)

	// pageSize is clamped so one request cannot dump the whole table.
	rec = httptest.NewRecorder()
	a.HandleModels(rec, httptest.NewRequest(http.MethodGet, "/api/models?pageSize=100000", nil))
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope), "clamped envelope decode failed: %v")
	testutil.Equal(t, envelope.PageSize, maxAdminModelPageSize)
}

func TestAdminWorkBuddyModelsFollowEnabledAccountEntitlements(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	ctx := context.Background()
	seedAdminModels(t, s,
		&store.Model{ID: "wb-free", Channel: "WorkBuddy", ModelID: "hy3", Status: store.ModelStatusAvailable},
		&store.Model{ID: "wb-paid", Channel: "WorkBuddy", ModelID: "gpt-6-astra", Status: store.ModelStatusAvailable},
		&store.Model{ID: "wb-missing", Channel: "WorkBuddy", ModelID: "hy4-preview-f", Status: store.ModelStatusAvailable},
		&store.Model{ID: "grok", Channel: "grok", ModelID: "grok-4.7", Status: store.ModelStatusAvailable},
	)
	paid := &store.Account{AccountType: "workbuddy", Enabled: true, UsageLimit: 100, UsageCurrent: 50}
	spent := &store.Account{AccountType: "workbuddy", Enabled: true, UsageLimit: 100, WorkBuddyModelIDs: []string{`{"id":"hy3","credits":"x0.00"}`, `{"id":"gpt-6-astra"}`}}
	testutil.CheckNoError(t, s.CreateAccount(ctx, paid))
	testutil.CheckNoError(t, s.CreateAccount(ctx, spent))
	read := func() []store.Model {
		t.Helper()
		rec := httptest.NewRecorder()
		a.HandleModels(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
		testutil.Equal(t, rec.Code, http.StatusOK)
		var models []store.Model
		testutil.CheckNoError(t, json.Unmarshal(rec.Body.Bytes(), &models))
		return models
	}
	testutil.Equal(t, len(read()), 4)
	paid.Enabled = false
	testutil.CheckNoError(t, s.UpdateAccount(ctx, paid))
	for _, status := range []string{"", store.AccountStatusWorkBuddyQuotaExhausted, "402"} {
		spent.StatusCode = status
		testutil.CheckNoError(t, s.UpdateAccount(ctx, spent))
		models := read()
		testutil.Equal(t, len(models), 2)
		for _, model := range models {
			if model.Channel == "WorkBuddy" && model.ModelID != "hy3" {
				t.Fatalf("non-entitled model remained visible: %s", model.ModelID)
			}
		}
	}
	// Pagination counts the projected list, while Redis retains all discoveries.
	rec := httptest.NewRecorder()
	a.HandleModels(rec, httptest.NewRequest(http.MethodGet, "/api/models?page=1&pageSize=1&search=hy", nil))
	var page adminModelListEnvelope
	testutil.CheckNoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	testutil.Equal(t, page.Total, 1)
	testutil.Equal(t, page.Items[0].ModelID, "hy3")
	stored, err := s.ListModels(ctx)
	testutil.CheckNoError(t, err)
	testutil.Equal(t, len(stored), 4)
	paid.Enabled = true
	testutil.CheckNoError(t, s.UpdateAccount(ctx, paid))
	testutil.Equal(t, len(read()), 4)
	paid.Enabled = false
	spent.Enabled = false
	testutil.CheckNoError(t, s.UpdateAccount(ctx, paid))
	testutil.CheckNoError(t, s.UpdateAccount(ctx, spent))
	testutil.Equal(t, len(read()), 1)
}

func TestAdminModelEntitlementsRespectAuthCatalogAndBinding(t *testing.T) {
	models := []*store.Model{
		{Channel: "workbuddy", ModelID: "alias", UpstreamModel: "hy3", BoundAccountIDs: []int64{1}},
		{Channel: "qoder", ModelID: "qwen"},
	}
	for _, tc := range []struct {
		name    string
		account store.Account
		want    int
	}{
		{"other channel", store.Account{ID: 1, AccountType: "qoder", Enabled: true}, 1},
		{"inactive auth", store.Account{ID: 1, AccountType: "workbuddy", Enabled: true, AuthStatus: "reauth_required", UsageCurrent: 10}, 1},
		{"refused credential", store.Account{ID: 1, AccountType: "workbuddy", Enabled: true, StatusCode: "401", UsageCurrent: 10}, 1},
		{"different binding", store.Account{ID: 2, AccountType: "workbuddy", Enabled: true, UsageCurrent: 10}, 1},
		{"bare catalog", store.Account{ID: 1, AccountType: "workbuddy", Enabled: true, StatusCode: "402", WorkBuddyModelIDs: []string{"hy3"}}, 1},
		{"missing billing marker", store.Account{ID: 1, AccountType: "workbuddy", Enabled: true, StatusCode: "402", WorkBuddyModelIDs: []string{`{"id":"hy3"}`}}, 1},
		{"formerly free now metered", store.Account{ID: 1, AccountType: "workbuddy", Enabled: true, StatusCode: "402", WorkBuddyModelIDs: []string{`{"id":"hy3","credits":"x0.03"}`}}, 1},
		{"free upstream", store.Account{ID: 1, AccountType: "workbuddy", Enabled: true, StatusCode: "402", WorkBuddyModelIDs: []string{`{"id":"hy3","credits":"x0.00"}`}}, 2},
		{"temporary throttle", store.Account{ID: 1, AccountType: "workbuddy", Enabled: true, StatusCode: "429", UsageCurrent: 10}, 2},
		{"unknown quota", store.Account{ID: 1, AccountType: "workbuddy", Enabled: true}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testutil.Equal(t, len(filterAdminModelEntitlements(models, []*store.Account{&tc.account})), tc.want)
		})
	}
}

func TestAdminGrokModelsUseBoundEnabledAccountCatalog(t *testing.T) {
	models := []*store.Model{{Channel: "grok", ModelID: "future-model"}, {Channel: "grok", ModelID: "old-derived-model"}, {Channel: "grok", ModelID: "bound", UpstreamModel: "future-model", BoundAccountIDs: []int64{2}}}
	accounts := []*store.Account{{ID: 1, AccountType: "grok", Enabled: true, GrokModels: []string{"future-model"}}, {ID: 2, AccountType: "grok", Enabled: false, GrokModels: []string{"future-model", "old-derived-model"}}}
	got := filterAdminModelEntitlements(models, accounts)
	testutil.Equal(t, len(got), 1)
	testutil.Equal(t, got[0].ModelID, "future-model")
	accounts[0].StatusCode = "401"
	testutil.Equal(t, len(filterAdminModelEntitlements(models, accounts)), 0)
}
