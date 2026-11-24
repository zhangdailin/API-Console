package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
)

//go:embed static/*
var staticFS embed.FS

//go:embed templates/*
var TemplateFS embed.FS

// assetVersionPlaceholder is what the static login page carries where a version
// string belongs; LoginPage replaces it at serve time.
const assetVersionPlaceholder = "__ASSET_VERSION__"

var embeddedAssetVersion = sync.OnceValue(func() string { return assetVersionFor(staticFS) })

// AssetVersion returns a short content hash of the embedded stylesheets and
// scripts.
//
// Every page template puts this on each <link> and <script> URL, and
// StaticHandler marks a versioned asset immutable for a year. A version string
// maintained by hand therefore has to be bumped by whoever edits the asset, and
// skipping that step pins browsers to the previous deployment: removing a
// provider from the UI changed the bytes but left the URL identical, so the
// retired provider stayed visible in a browser that had already cached it.
// Hashing the bytes makes the URL change whenever the bytes do.
func AssetVersion() string {
	return embeddedAssetVersion()
}

func assetVersionFor(assets fs.FS) string {
	sum := sha256.New()
	_ = fs.WalkDir(assets, "static", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		switch strings.ToLower(path.Ext(name)) {
		case ".css", ".js":
		default:
			return nil
		}
		data, readErr := fs.ReadFile(assets, name)
		if readErr != nil {
			return readErr
		}
		fmt.Fprintf(sum, "%s\x00%d\x00", name, len(data))
		_, _ = sum.Write(data)
		return nil
	})
	return hex.EncodeToString(sum.Sum(nil))[:12]
}

// LoginPage returns the embedded admin login page with the asset-version
// placeholder resolved. The page is a plain static file rather than a parsed
// template, so it cannot use the PageData field the page templates read.
func LoginPage() ([]byte, error) {
	raw, err := staticFS.ReadFile("static/login.html")
	if err != nil {
		return nil, err
	}
	return bytes.ReplaceAll(raw, []byte(assetVersionPlaceholder), []byte(AssetVersion())), nil
}

func StaticHandler() http.Handler {
	subFS, _ := fs.Sub(staticFS, "static")
	files := http.FileServer(http.FS(subFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ext := strings.ToLower(path.Ext(r.URL.Path))
		versioned := strings.TrimSpace(r.URL.Query().Get("v")) != ""
		switch {
		case versioned && isImmutableAsset(ext):
			// Asset URLs carry an explicit version query in every page template.
			// Let browsers retain those immutable bytes instead of downloading the
			// shared stylesheet and scripts on every admin navigation.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			// HTML entry points and unversioned assets must continue to pick up a
			// deployment that changes their content at the same URL.
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func isImmutableAsset(ext string) bool {
	switch ext {
	case ".css", ".js", ".svg", ".png", ".jpg", ".jpeg", ".webp", ".gif", ".ico", ".woff", ".woff2":
		return true
	default:
		return false
	}
}
