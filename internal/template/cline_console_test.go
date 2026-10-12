package template

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

func TestAccountModalOffersClineLogin(t *testing.T) {
	renderer, err := NewRenderer()
	testutil.NoError(t, err, "NewRenderer() error = %v")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?tab=accounts", nil)
	testutil.NoError(t, renderer.RenderIndex(recorder, request, &config.Config{AdminPath: "/admin"}, nil), "RenderIndex() error = %v")
	page := recorder.Body.String()
	for _, want := range []string{
		`id="clineLoginGroup"`,
		`id="clineLoginButton"`,
		`data-login-provider="cline"`,
		`js/cline-auth.js`,
	} {
		testutil.CheckContain(t, page, want)
	}
}

func TestModelModalOffersClineChannel(t *testing.T) {
	renderer, err := NewRenderer()
	testutil.NoError(t, err, "NewRenderer() error = %v")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?tab=models", nil)
	testutil.NoError(t, renderer.RenderIndex(recorder, request, &config.Config{AdminPath: "/admin"}, nil), "RenderIndex() error = %v")
	page := recorder.Body.String()
	testutil.CheckFalse(t, !strings.Contains(page, `provider-registry.js`) || !strings.Contains(page, `id="modelChannel"`), "the model modal is not wired to the shared provider registry")
}
