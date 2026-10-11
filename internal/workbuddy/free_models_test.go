package workbuddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/store"
	"testing"
)

func TestFreeEntitlementRequiresObservedZeroCredits(t *testing.T) {
	for _, tc := range []struct {
		row  string
		id   string
		want bool
	}{
		{`{"id":"new-model","credits":"x0.00"}`, "NEW-MODEL", true},
		{`{"id":"hy3"}`, "hy3", false},
		{`{"id":"hy3","credits":"x0.03"}`, "hy3", false},
		{`{"id":"glm-5.3-flash","credits":"x0.06"}`, "glm-5.3-flash", false},
		{`{"id":"glm-5.3-flash","credits":"x0.00"}`, "glm-5.3-flash", true},
		{`{"id":"deepseek-v4.1-flash-sg","credits":"x0.03"}`, "deepseek-v4.1-flash-sg", false},
		{`{"id":"deepseek-v4.1-flash-sg","credits":"x0.00"}`, "deepseek-v4.1-flash-sg", true},
		{`{"id":"new-model","credits":"0x"}`, "new-model", true},
		{`{"id":"new-model","credits":"Free now"}`, "new-model", false},
		{`{"id":"new-model","credits":"x-0"}`, "new-model", false},
		{`{"id":"new-model","credits":"xNaN"}`, "new-model", false},
		{`{"id":"new-model","credits":"x0e-900"}`, "new-model", false},
		{`{"id":"new-model","credits":null}`, "new-model", false},
		{`{"id":"new-model","credits":0}`, "new-model", false},
		{`"hy3"`, "hy3", false},
		{`{"id":"other","credits":"x0.00"}`, "new-model", false},
	} {
		if got := IsFreeModelInCatalog([]string{tc.row}, tc.id); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.row, got, tc.want)
		}
	}
}

func TestCreditsAreNotOverriddenByPromotionRules(t *testing.T) {
	rows := []string{
		`{"id":"arbitrary-model","credits":"x0.00","model_promotions":[{"enabled":false,"kind":"discount","priority":999,"discount":{"factor":1,"discountedCredits":"x0.03"},"schedule":{"validUntil":"2000-01-01T00:00:00Z"}}]}`,
		`{"id":"paid","credits":"x0.03","model_promotions":[{"enabled":true,"kind":"discount","discount":{"factor":0,"discountedCredits":"0x"}}]}`,
	}
	if !IsFreeModelInCatalog(rows, "arbitrary-model") {
		t.Fatal("extra promotion rules hid an upstream zero-credit model")
	}
	if IsFreeModelInCatalog(rows, "paid") {
		t.Fatal("promotion overrode nonzero upstream credits")
	}
}

func TestFetchModelsPreservesUpstreamBillingMarkers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"future-model","credits":"x0.00"},{"id":"paid","credits":"x0.03"},{"id":"unknown"}],"modelPromotions":[{"kind":"discount","enabled":true,"modelIds":["paid"],"discount":{"factor":0,"discountedCredits":"0x"}}],"agents":[{"name":"cli","models":["future-model","paid","unknown"]}]}}`))
	}))
	defer srv.Close()
	client := NewFromAccount(&store.Account{}, nil)
	client.baseURL, client.httpClient = srv.URL, srv.Client()
	client.creds = Credentials{AccessToken: "access", UID: "uid"}
	models, err := client.FetchModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows := CatalogSnapshot(models)
	if !IsFreeModelInCatalog(rows, "future-model") || IsFreeModelInCatalog(rows, "paid") || IsFreeModelInCatalog(rows, "unknown") {
		t.Fatal("upstream billing markers lost in fetch/snapshot path")
	}
}
