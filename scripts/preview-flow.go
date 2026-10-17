//go:build ignore

// Render the embedded operations page locally, without credentials or API data.
// Browser verification supplies explicit simulated observations.
package main

import (
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"orchids-api/web"
)

func main() {
	port := flag.Int("port", 3099, "loopback preview port")
	flag.Parse()
	templates := template.New("pages")
	err := fs.WalkDir(web.TemplateFS, "templates", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".html") {
			return nil
		}
		raw, readErr := web.TemplateFS.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		_, parseErr := templates.New(filepath.Base(path)).Parse(string(raw))
		return parseErr
	})
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/admin/", http.StripPrefix("/admin", web.StaticHandler()))
	mux.HandleFunc("/preview", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.ExecuteTemplate(w, "page-ops", map[string]any{"AdminPath": "/admin", "AssetVersion": web.AssetVersion(), "ActiveTab": "ops", "Title": "API Console · Flow Preview"}); err != nil {
			log.Print(err)
		}
	})
	address := fmt.Sprintf("127.0.0.1:%d", *port)
	log.Printf("Flow preview: http://%s/preview (no API data; use verify-flow-screen.cjs)", address)
	log.Fatal(http.ListenAndServe(address, mux))
}
