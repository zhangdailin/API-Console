package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestAssetVersionIsContentDerived(t *testing.T) {
	const script, stylesheet = "static/js/example.js", "static/css/example.css"
	fixture := func() fstest.MapFS {
		return fstest.MapFS{
			script:     {Data: []byte("let n = 1;")},
			stylesheet: {Data: []byte("p { color: blue; }")},
		}
	}
	base := assetVersionFor(fixture())
	if again := assetVersionFor(fixture()); base == "" || again != base {
		t.Fatalf("identical assets did not produce a stable version: %q, %q", base, again)
	}
	for path, changed := range map[string]string{
		script: "let n = 2;", stylesheet: "p { color: cyan; }",
	} {
		t.Run(path, func(t *testing.T) {
			assets := fixture()
			assets[path].Data = []byte(changed)
			if got := assetVersionFor(assets); got == base {
				t.Fatalf("editing %s did not invalidate the asset version %q", path, base)
			}
		})
	}
	if got, want := AssetVersion(), assetVersionFor(staticFS); got != want {
		t.Fatalf("embedded version = %q, want %q from the shipped assets", got, want)
	}
}

// TestLoginPageResolvesAssetVersion covers the static login page, which is the
// one asset URL that cannot read PageData.
func TestLoginPageResolvesAssetVersion(t *testing.T) {
	page, err := LoginPage()
	if err != nil {
		t.Fatalf("LoginPage() error = %v", err)
	}
	if bytes.Contains(page, []byte(assetVersionPlaceholder)) {
		t.Fatalf("LoginPage() still carries %q", assetVersionPlaceholder)
	}
	want := []byte("main.css?v=" + AssetVersion())
	if !bytes.Contains(page, want) {
		t.Fatalf("LoginPage() does not link %s", want)
	}
	if !strings.Contains(string(page), "<title>") {
		t.Fatal("LoginPage() does not look like the login page")
	}
}

func TestStaticHandlerCachePolicy(t *testing.T) {
	handler := StaticHandler()
	tests := []struct {
		path string
		want string
	}{
		{"/css/main.css?v=release", "public, max-age=31536000, immutable"},
		{"/js/common.js?v=release", "public, max-age=31536000, immutable"},
		{"/css/main.css", "no-cache"},
		{"/login.html", "no-cache"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)
			handler.ServeHTTP(recorder, request)
			if got := recorder.Header().Get("Cache-Control"); got != tt.want {
				t.Fatalf("Cache-Control = %q, want %q", got, tt.want)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
		})
	}
}
