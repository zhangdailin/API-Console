package main

import (
	"context"
	"log/slog"
	"net/http"
	"orchids-api/internal/modelrefresh"
	"orchids-api/internal/responses"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/api"
	"orchids-api/internal/auth"
	"orchids-api/internal/buildinfo"
	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/selfupdate"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
	"orchids-api/web"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// registerWithPrefixes registers the same handler under multiple prefix+path combinations.
func registerWithPrefixes(mux *http.ServeMux, prefixes []string, path string, h http.HandlerFunc) {
	for _, p := range prefixes {
		mux.HandleFunc(p+path, h)
	}
}

func registerRoutes(
	rootMux *http.ServeMux,
	cfg *config.Config,
	s *store.Store,
	h *handler.Handler,
	grokHandler *grok.Handler,
	apiHandler *api.API,
	limiter *middleware.ConcurrencyLimiter,
	accountTracker loadbalancer.ConnTracker,
	tmplRenderer *template.Renderer,
) *selfupdate.Manager {
	mux := http.NewServeMux()
	currentConfig := func() *config.Config {
		if current := apiHandler.ConfigSnapshot(); current != nil {
			return current
		}
		return cfg
	}
	modelRefreshHandler := modelrefresh.NewRefreshHandler(currentConfig, s)
	var anonymousCache middleware.AnonymousAllowlistCache
	providerAdmission := middleware.ProviderAdmission(func() map[string]int {
		current := currentConfig()
		if current == nil {
			return nil
		}
		return current.ProviderConcurrencyLimits
	})
	coalesceFlush := middleware.CoalesceStreamFlush(func() time.Duration {
		current := currentConfig()
		ms := 2
		if current != nil && current.StreamFlushIntervalMs != 0 {
			ms = current.StreamFlushIntervalMs
		}
		if ms < 0 {
			return 0
		}
		return time.Duration(min(ms, 20)) * time.Millisecond
	})
	inferenceAuth := func(next http.HandlerFunc) http.HandlerFunc {
		authenticated := middleware.APIKeyAuthWithRequest(
			// A key is required on every provider inference route; `inference_auth_enabled: false`
			// used to open every
			// inference route to anonymous callers and is now advisory only. The one
			// exception is an explicit anonymous_allow_ips source, which a deployment
			// names when it cannot yet update that client.
			func(r *http.Request) bool {
				cfg := currentConfig()
				if cfg == nil {
					return true
				}
				allowlist, err := anonymousCache.Get(cfg.AnonymousAllowIPs)
				if err != nil {
					// A malformed entry makes the list unusable: require keys rather
					// than silently opening the routes.
					slog.Warn("anonymous_allow_ips is invalid; requiring a key from everyone", "error", err)
					return true
				}
				if allowlist.Allows(r) {
					slog.Debug("anonymous inference request allowed by anonymous_allow_ips", "client_ip", middleware.ClientIP(r))
					return false
				}
				return true
			},
			func(ctx context.Context, token string) (*middleware.APIKeyPrincipal, error) {
				key, err := s.AuthorizeApiKey(ctx, token)
				switch {
				case err == nil:
					return &middleware.APIKeyPrincipal{
						ID:                   key.ID,
						AllowedModels:        key.AllowedModels,
						MaxConcurrent:        key.MaxConcurrent,
						BillingLimitUSDTicks: key.BillingLimitUSDTicks,
					}, nil
				case err == store.ErrNoRows:
					return nil, nil
				case err == store.ErrApiKeyExpired:
					return &middleware.APIKeyPrincipal{DenialCode: middleware.APIKeyDenialExpired}, nil
				case err == store.ErrApiKeyRateLimited:
					return &middleware.APIKeyPrincipal{DenialCode: middleware.APIKeyDenialRateLimited}, nil
				default:
					return nil, err
				}
			},
			// Billing is reserved after admission (a request rejected for
			// concurrency must not hold budget) and before the handler runs, so a
			// key whose limit cannot cover the request is answered 402 instead.
			middleware.APIKeyConcurrencyWithTracker(
				middleware.APIKeyBillingReservation(middleware.InferenceBudget(currentConfig)(next), s, middleware.DefaultBillingReservationTTL),
				accountTracker,
			),
		)
		// Admit once, before Redis authentication/billing work.
		return limiter.Limit(providerAdmission(coalesceFlush(authenticated)))
	}
	// All inference routes bind a provider by path, with or without /v1.
	channelPrefixes := channel.GenericPrefixes()
	allPrefixes := channel.AllPrefixes()

	// --- Channel-specific message routes ---
	// Every channel answers the same two endpoints; the path only tells the
	// handler which channel's token profile and account pool to use.
	registerWithPrefixes(mux, channelPrefixes, "/messages", inferenceAuth(h.HandleMessages))
	registerWithPrefixes(mux, channelPrefixes, "/messages/count_tokens", inferenceAuth(h.HandleCountTokens))

	// --- Model routes (channel prefixes → same handlers) ---
	registerWithPrefixes(mux, allPrefixes, "/models", inferenceAuth(h.HandleModels))
	registerWithPrefixes(mux, allPrefixes, "/models/", inferenceAuth(h.HandleModelByID))

	// --- OpenAI-compatible channel chat routes ---
	registerWithPrefixes(mux, channelPrefixes, "/chat/completions", inferenceAuth(h.HandleMessages))

	// --- OpenAI Responses API for the chat-completions-only channels ---
	// Codex defaults to the Responses wire API, so without this bridge every
	// channel except Grok answers 404 on /responses. The bridge forwards to the
	// same channel's chat handler, which keeps account selection and retries in
	// one place. Records are persisted in the shared response store, so
	// store=true, previous_response_id and GET/DELETE /responses/{id} work here
	// too. The subtree below /responses/ carries the sibling endpoints
	// (trailing slash, compact, resource retrieval).
	responseStoreTTL := time.Duration(0)
	if cfg != nil && cfg.ResponseStoreTTL > 0 {
		responseStoreTTL = time.Duration(cfg.ResponseStoreTTL) * time.Hour
	}
	bridgeOptions := responses.BridgeOptions{Store: s, TTL: responseStoreTTL}
	channelResponses := grok.ResponsesBridgeHandler(h.HandleMessages, bridgeOptions)
	channelResponsesSub := grok.ResponsesChannelSubpath(h.HandleMessages, bridgeOptions)
	registerWithPrefixes(mux, channelPrefixes, "/responses", inferenceAuth(channelResponses))
	registerWithPrefixes(mux, channelPrefixes, "/responses/", inferenceAuth(channelResponsesSub))
	// Resource actions share the same ownership-checked store on both bases.
	registerWithPrefixes(mux, allPrefixes, "/responses/{response_id}/cancel",
		inferenceAuth(responses.CancelHandler(bridgeOptions)))
	registerWithPrefixes(mux, allPrefixes, "/responses/{response_id}/input_items",
		inferenceAuth(responses.InputItemsHandler(bridgeOptions)))

	grokPrefixes := channel.PrefixesFor(channel.Grok)
	registerWithPrefixes(mux, grokPrefixes, "/chat/completions", inferenceAuth(grokHandler.HandleChatCompletions))
	registerWithPrefixes(mux, grokPrefixes, "/messages", inferenceAuth(grokHandler.HandleMessages))
	registerWithPrefixes(mux, grokPrefixes, "/messages/count_tokens", inferenceAuth(h.HandleCountTokens))
	// Grok uses the native Build Responses implementation.
	registerWithPrefixes(mux, grokPrefixes, "/responses", inferenceAuth(grokHandler.HandleResponses))
	registerWithPrefixes(mux, grokPrefixes, "/responses/compact", inferenceAuth(grokHandler.HandleResponsesCompact))
	registerWithPrefixes(mux, grokPrefixes, "/responses/", inferenceAuth(grokHandler.HandleResponseResource))
	// --- Public auth/login (no prefix duplication) ---
	mux.HandleFunc("/api/login", apiHandler.HandleLogin)
	mux.HandleFunc("/api/logout", apiHandler.HandleLogout)

	// --- Admin API routes (session auth) ---
	sessionAuth := func(h http.HandlerFunc) http.HandlerFunc {
		return middleware.SessionAuthDynamic(func() (string, string) {
			current := currentConfig()
			return current.AdminPass, current.AdminToken
		}, h)
	}

	// Admin routes under /api/* only (no dual prefix)
	mux.HandleFunc("/api/providers", sessionAuth(channel.HandleRegistry))
	mux.HandleFunc("/api/accounts", sessionAuth(apiHandler.HandleAccounts))
	mux.HandleFunc("/api/accounts/", sessionAuth(apiHandler.HandleAccountByID))
	mux.HandleFunc("/api/workbuddy/login", sessionAuth(apiHandler.HandleWorkBuddyLogin))
	mux.HandleFunc("/api/workbuddy/login/", sessionAuth(apiHandler.HandleWorkBuddyLogin))
	mux.HandleFunc("/api/qoder/login", sessionAuth(apiHandler.HandleQoderLogin))
	mux.HandleFunc("/api/qoder/login/", sessionAuth(apiHandler.HandleQoderLogin))
	mux.HandleFunc("/api/cline/login", sessionAuth(apiHandler.HandleClineLogin))
	mux.HandleFunc("/api/cline/login/", sessionAuth(apiHandler.HandleClineLogin))
	mux.HandleFunc("/api/grok/device-auth", sessionAuth(apiHandler.HandleGrokDeviceAuthorization))
	mux.HandleFunc("/api/grok/device-auth/", sessionAuth(apiHandler.HandleGrokDeviceAuthorization))
	mux.HandleFunc("/api/keys", sessionAuth(apiHandler.HandleKeys))
	mux.HandleFunc("/api/keys/", sessionAuth(apiHandler.HandleKeyByID))
	// POST /api/keys/{id}/reset-usage lands on the same handler, which dispatches
	// on the trailing path segment.
	mux.HandleFunc("/api/models", sessionAuth(apiHandler.HandleModels))
	mux.HandleFunc("/api/models/refresh", sessionAuth(modelRefreshHandler))
	mux.HandleFunc("/api/models/", sessionAuth(apiHandler.HandleModelByID))
	mux.HandleFunc("/api/export", sessionAuth(apiHandler.HandleExport))
	mux.HandleFunc("/api/import", sessionAuth(apiHandler.HandleImport))
	updater := selfupdate.New(cfg.Port)
	mux.HandleFunc("/api/system/version", sessionAuth(updater.HandleVersion))
	mux.HandleFunc("/api/system/check-updates", sessionAuth(updater.HandleCheck))
	mux.HandleFunc("/api/system/operation", sessionAuth(updater.HandleStatus))
	mux.HandleFunc("/api/system/update", sessionAuth(updater.HandleAction("update")))
	mux.HandleFunc("/api/system/rollback", sessionAuth(updater.HandleAction("rollback")))
	mux.HandleFunc("/api/config/list", sessionAuth(apiHandler.HandleConfigList))
	mux.HandleFunc("/api/config/save", sessionAuth(apiHandler.HandleConfigSave))
	// Operations monitoring: the overview, the channel × model matrix and the
	// alert set behind the operations overview page.
	mux.HandleFunc("/api/ops/overview", sessionAuth(apiHandler.HandleOpsOverview))
	mux.HandleFunc("/api/ops/alerts/rules", sessionAuth(apiHandler.HandleOpsAlertRules))
	mux.HandleFunc("/api/ops/runtime", sessionAuth(apiHandler.HandleOpsRuntime))
	// Journal: one filtered endpoint for request, operation, and system entries,
	// with the upstream attempts of each request joined in.
	mux.HandleFunc("/api/journal/records", sessionAuth(apiHandler.HandleJournalRecords))
	mux.HandleFunc("/api/journal/diagnostics", sessionAuth(apiHandler.HandleJournalDiagnostics))
	mux.HandleFunc("/api/journal/diagnostics/settings", sessionAuth(apiHandler.HandleDiagnosticSettings))

	// --- Static assets ---
	staticRootHandler := web.StaticHandler()
	mux.Handle("/static/", http.StripPrefix("/static/", staticRootHandler))

	// The public Grok conversation page was retired. Root traffic now enters the
	// authenticated admin UI; the old public/chat/media aliases are intentionally
	// left unregistered and therefore return 404.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, cfg.AdminPath+"/", http.StatusFound)
	})

	// --- Admin Web UI ---
	registerAdminUI(mux, cfg, currentConfig, s, staticRootHandler, tmplRenderer)

	// --- Health, metrics, pprof ---
	// /health reports every registered provider. The set comes from the channel
	// registry rather than a literal, so removing a provider removes its key
	// here too, and a provider that is not configured cannot be reported as one.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		providers := make(map[string]string, len(channel.All()))
		for _, definition := range channel.All() {
			providers[string(definition.ID)] = "ready"
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":    "ok",
			"providers": providers,
			"build":     buildinfo.Current(),
		})
	})
	mux.Handle("/metrics", promhttp.Handler())
	slog.Debug("Prometheus metrics enabled", "path", "/metrics")

	if cfg.DebugEnabled {
		mux.HandleFunc("/debug/pprof/", middleware.SessionAuthDynamic(func() (string, string) {
			current := currentConfig()
			return current.AdminPass, current.AdminToken
		}, http.DefaultServeMux.ServeHTTP))
		slog.Debug("pprof enabled", "path", "/debug/pprof/")
	}
	rootMux.Handle("/", mux)
	return updater
}

func registerAdminUI(mux *http.ServeMux, cfg *config.Config, currentConfig func() *config.Config, s *store.Store, staticRootHandler http.Handler, tmplRenderer *template.Renderer) {
	staticHandler := http.StripPrefix(cfg.AdminPath, staticRootHandler)
	currentUIConfig := func() *config.Config {
		current := currentConfig().Clone()
		// The route tree cannot be re-registered while the server is running.
		// AdminPath therefore remains a restart-required setting even though the
		// rest of the rendered configuration is refreshed immediately.
		current.AdminPath = cfg.AdminPath
		return current
	}

	isAdminAuthenticated := func(r *http.Request) bool {
		cookie, err := r.Cookie("session_token")
		authenticated := err == nil && auth.ValidateSessionToken(cookie.Value)
		if authenticated {
			return true
		}
		adminToken := currentConfig().AdminToken
		authHeader := r.Header.Get("Authorization")
		return adminToken != "" && (authHeader == "Bearer "+adminToken || authHeader == adminToken || r.Header.Get("X-Admin-Token") == adminToken)
	}
	renderAdminIndex := func(w http.ResponseWriter, r *http.Request) {
		if err := tmplRenderer.RenderIndex(w, r, currentUIConfig(), s); err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}

	mux.HandleFunc(cfg.AdminPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		http.Redirect(w, r, cfg.AdminPath+"/", http.StatusFound)
	})
	// The login page is a plain static file, so it cannot read the PageData field
	// the rendered pages use for ?v=; web.LoginPage resolves the same content-hash
	// placeholder inside it instead.
	serveLoginPage := func(w http.ResponseWriter, r *http.Request) {
		page, err := web.LoginPage()
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(page)
	}
	mux.HandleFunc(cfg.AdminPath+"/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serveLoginPage(w, r)
	})

	// All seven admin pages are selected by ?tab=... on the index route.

	mux.HandleFunc(cfg.AdminPath+"/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == cfg.AdminPath+"/login.html" {
			serveLoginPage(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, cfg.AdminPath+"/css/") ||
			strings.HasPrefix(r.URL.Path, cfg.AdminPath+"/js/") {
			staticHandler.ServeHTTP(w, r)
			return
		}
		if !isAdminAuthenticated(r) {
			http.Redirect(w, r, cfg.AdminPath+"/login.html", http.StatusFound)
			return
		}
		if r.URL.Path == cfg.AdminPath+"/" || r.URL.Path == cfg.AdminPath {
			renderAdminIndex(w, r)
			return
		}
		staticHandler.ServeHTTP(w, r)
	})
}
