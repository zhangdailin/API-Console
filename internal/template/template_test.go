package template

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// TestRenderIndexShipsNoCompetingSidebarCount pins the fix for the sidebar that
// read 5 on 账号管理 and 11 on 运维总览. Three rules used to write #footerAbnormal
// (this renderer counted !Enabled, accounts.js counted its own verdict,
// common.js counted a third). The renderer must now ship no number at all, so
// common.js's single predicate over /api/accounts is the only source.
func TestRenderIndexShipsNoCompetingSidebarCount(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "template_test:"})
	testutil.NoError(t, err, "store.New() error = %v")
	defer func() { _ = s.Close() }()

	accounts := []*store.Account{
		{AccountType: "grok", CredentialType: "oauth", GrokProvider: "build", Enabled: true},
		{AccountType: "grok", CredentialType: "oauth", GrokProvider: "build", Enabled: false},
	}
	for _, acc := range accounts {
		testutil.NoError(t, s.CreateAccount(context.Background(), acc), "CreateAccount() error = %v")
	}

	renderer, err := NewRenderer()
	testutil.NoError(t, err, "NewRenderer() error = %v")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?tab=accounts", nil)
	testutil.NoError(t, renderer.RenderIndex(recorder, request, &config.Config{AdminPath: "/admin"}, s), "RenderIndex() error = %v")
	body := recorder.Body.String()
	for _, element := range []string{`id="footerTotal"`, `id="footerNormal"`, `id="footerAbnormal"`} {
		testutil.CheckContain(t, body, element+`>—</span>`)
	}
	// A server-side count would have printed the two rows and one disabled row;
	// neither number may appear.
	testutil.MustNotContainAny(t, body, `id="footerTotal">2</span>`, `id="footerAbnormal">1</span>`)
}

func TestTutorialRedirectsToKeys(t *testing.T) {
	renderer, err := NewRenderer()
	testutil.NoError(t, err)
	recorder := httptest.NewRecorder()
	err = renderer.RenderIndex(recorder, httptest.NewRequest("GET", "/?tab=tutorial", nil), &config.Config{AdminPath: "/console"}, nil)
	testutil.NoError(t, err)
	testutil.Equal(t, recorder.Code, http.StatusSeeOther)
	testutil.Equal(t, recorder.Header().Get("Location"), "/console/?tab=keys&section=auth")
}

func TestPagesRenderOnlyTheirOwnModals(t *testing.T) {
	renderer, err := NewRenderer()
	testutil.NoError(t, err, "NewRenderer() error = %v")

	tests := []struct {
		tab       string
		wantIDs   []string
		forbidIDs []string
	}{
		{"accounts", []string{"accountModal"}, []string{"modelModal", "createKeyModal"}},
		{"keys", []string{"createKeyModal", "editKeyModal", "showKeyModal", "deleteKeyModal"}, []string{"accountModal", "modelModal"}},
		{"models", []string{"modelModal"}, []string{"accountModal", "createKeyModal"}},
	}

	for _, tt := range tests {
		t.Run(tt.tab, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/?tab="+tt.tab, nil)
			testutil.NoError(t, renderer.RenderIndex(recorder, request, &config.Config{AdminPath: "/admin"}, nil), "RenderIndex() error = %v")
			page := recorder.Body.String()
			for _, id := range tt.wantIDs {
				testutil.CheckContain(t, page, `id="`+id+`"`)
			}
			for _, id := range tt.forbidIDs {
				testutil.CheckNotContain(t, page, `id="`+id+`"`)
			}
		})
	}
}

func TestSidebarUsesRealLinks(t *testing.T) {
	renderer, err := NewRenderer()
	testutil.NoError(t, err, "NewRenderer() error = %v")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?tab=ops", nil)
	testutil.NoError(t, renderer.RenderIndex(recorder, request, &config.Config{AdminPath: "/console"}, nil), "RenderIndex() error = %v")
	page := recorder.Body.String()
	for _, tab := range []string{"ops", "logs", "accounts", "keys", "models", "alerts"} {
		testutil.CheckContain(t, page, `href="/console/?tab=`+tab+`"`)
	}
	testutil.CheckNotContain(t, page, `onclick="switchTab(`)
}

// TestEveryPageRendersTheSharedDocumentHead renders each page through the real
// embedded templates. The <head> used to be copy-pasted into every page and is
// now one partial, so a page that forgets the include would silently lose its
// stylesheet and title; this pins the include and the page's own marker.
func TestEveryPageRendersTheSharedDocumentHead(t *testing.T) {
	renderer, err := NewRenderer()
	testutil.NoError(t, err, "NewRenderer() error = %v")

	pages := map[string]string{
		"ops":      "opsAlertRules",
		"logs":     "filterActor",
		"alerts":   "alertsEvents",
		"models":   "currentChannelPill",
		"keys":     "authConfig",
		"accounts": "accountsList",
	}
	for tab, marker := range pages {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/?tab="+tab, nil)
		err := renderer.RenderIndex(recorder, request, &config.Config{AdminPath: "/admin"}, nil)
		testutil.CheckNoError(t, err)
		page := recorder.Body.String()
		for _, want := range []string{"<!DOCTYPE html>", "/admin/css/main.css?v=", "</head>", `id="` + marker + `"`} {
			testutil.CheckContain(t, page, want)
		}
	}
}
