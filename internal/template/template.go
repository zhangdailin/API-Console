package template

import (
	"html/template"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/web"
)

// Renderer handles template rendering
type Renderer struct {
	templates *template.Template
}

// NewRenderer creates a new template renderer
func NewRenderer() (*Renderer, error) {
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}

	return &Renderer{templates: tmpl}, nil
}

// parseTemplates parses all template files from the embedded filesystem
func parseTemplates() (*template.Template, error) {
	tmpl := template.New("")

	// Parse all templates recursively
	err := fs.WalkDir(web.TemplateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".html") {
			return nil
		}

		content, err := fs.ReadFile(web.TemplateFS, path)
		if err != nil {
			return err
		}

		// Use the filename without extension as template name
		name := filepath.Base(path)
		_, err = tmpl.New(name).Parse(string(content))
		return err
	})

	return tmpl, err
}

// RenderIndex renders the main index page
func (r *Renderer) RenderIndex(w http.ResponseWriter, req *http.Request, cfg *config.Config, s *store.Store) error {
	activeTab := getActiveTab(req)
	if activeTab == "tutorial" {
		http.Redirect(w, req, cfg.AdminPath+"/?tab=keys&section=auth", http.StatusSeeOther)
		return nil
	}

	// The sidebar account counters deliberately ship no server-side number. The
	// page used to count !Enabled here, the accounts page counted its own verdict
	// in accounts.js, and every other page counted common.js's verdict — three
	// rules writing one element, so the same sidebar read differently per tab.
	// common.js refreshSidebarAccountStats (and accounts.js on its own page) now
	// own the number from the /api/accounts payload, which is the single place
	// that knows credentials, status codes and quota state.

	data := &PageData{
		Title:        "API 管理面板",
		AdminPath:    cfg.AdminPath,
		ActiveTab:    activeTab,
		AssetVersion: web.AssetVersion(),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Render different page templates based on active tab
	var templateName string
	switch activeTab {
	case "ops":
		templateName = "page-ops"
	case "logs":
		templateName = "page-logs"
	case "alerts":
		templateName = "page-alerts"
	case "models":
		templateName = "page-models"
	case "keys":
		templateName = "page-config"
	case "accounts":
		templateName = "page-accounts"
	default:
		templateName = "page-accounts"
	}

	return r.templates.ExecuteTemplate(w, templateName, data)
}

// getActiveTab extracts the active tab from the request. The operations overview
// is the landing page: an operator opening the panel should see whether anything
// is on fire before anything else.
func getActiveTab(req *http.Request) string {
	tab := req.URL.Query().Get("tab")
	if tab == "" {
		tab = "ops"
	}
	return tab
}
