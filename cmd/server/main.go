package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"orchids-api/internal/modelrefresh"
	"orchids-api/internal/refresh"
	"os"
	"os/signal"
	"syscall"
	"time"

	"orchids-api/internal/accountevents"
	"orchids-api/internal/alerting"
	"orchids-api/internal/api"
	"orchids-api/internal/audit"
	"orchids-api/internal/auth"
	"orchids-api/internal/buildinfo"
	"orchids-api/internal/cline"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/grok"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/logutil"
	"orchids-api/internal/middleware"
	"orchids-api/internal/opsagg"
	"orchids-api/internal/provider"
	"orchids-api/internal/qoder"
	"orchids-api/internal/secureblob"
	"orchids-api/internal/selfupdate"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
	"orchids-api/internal/workbuddy"
)

// wiredOps is the per-minute aggregator created during startup. It is what the
// overview endpoints and the alert evaluator read; it stays nil on a deployment
// without Redis, and the API reports "no sample" in that case.
var wiredOps *opsagg.Aggregator

// alertEngine evaluates the alert rules after startup. A nil engine (no Redis)
// simply reports no alerts.
var alertEngine *alerting.Engine

// accountChangeEmitter adapts the store's change description to the notification
// bus. The store publishes only after a successful write; the bus decides what
// kind of change it was.
type accountChangeEmitter struct {
	bus *accountevents.Bus
}

// Publish implements store.ChangeEmitter. It resolves the after-state on its own
// goroutine (the store hands over only the id and the previous state), stamps the
// change kind, and hands it to the bus.
func (e accountChangeEmitter) Publish(change store.AccountChange) {
	if e.bus == nil || change.AccountID == 0 {
		return
	}
	var current *store.Account
	if wiredStore != nil {
		if loaded, err := wiredStore.GetAccount(context.Background(), change.AccountID); err == nil {
			current = loaded
		}
	}
	e.bus.Publish(accountevents.Change{
		AccountID: change.AccountID,
		Kind:      accountevents.Classify(change.Previous, current),
		Origin:    change.Origin,
	})
}

// wiredStore is the account store created during startup; the change emitter uses
// it to resolve the after-state of a mutation it was told about.
var wiredStore *store.Store

// wiredAuditLogger is the journal created at startup, used by the background
// loops that record system events.
var wiredAuditLogger audit.Logger

func main() {
	if handled, err := selfupdate.RunWatchdogCLI(os.Args[1:]); handled {
		if err != nil {
			slog.Error("Upgrade watchdog failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		_ = json.NewEncoder(os.Stdout).Encode(buildinfo.Current())
		return
	}
	configPath := flag.String("config", "", "Path to config.json/config.yaml")
	flag.Parse()

	cfg, resolvedConfigPath, err := config.Load(*configPath)
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	configureRuntimeLogging(cfg)
	credentialKey, credentialKeySource, err := config.LoadOrCreateCredentialEncryptionKey(resolvedConfigPath, cfg)
	if err != nil {
		slog.Error("Failed to load credential encryption key", "error", err)
		os.Exit(1)
	}
	slog.Info("Credential encryption enabled", "key_source", credentialKeySource)

	s, err := store.New(store.Options{
		RedisAddr:               cfg.RedisAddr,
		RedisPassword:           cfg.RedisPassword,
		RedisDB:                 cfg.RedisDB,
		RedisPoolSize:           cfg.RedisPoolSize,
		RedisPrefix:             cfg.RedisPrefix,
		CredentialEncryptionKey: credentialKey,
	})
	if err != nil {
		slog.Error("Failed to initialize database", "error", err)
		os.Exit(1)
	}
	defer s.Close()

	slog.Debug("Store initialized", "mode", "redis", "addr", cfg.RedisAddr, "prefix", cfg.RedisPrefix)

	// Load the saved config from Redis when one is present.
	if savedConfig, err := s.GetSetting(context.Background(), "config"); err == nil && savedConfig != "" {
		savedCfg := cfg.Clone()
		if err := json.Unmarshal([]byte(savedConfig), savedCfg); err != nil {
			slog.Warn("Failed to load config from Redis, using file config", "error", err)
		} else if err := config.ValidateProxyConfig(savedCfg); err != nil {
			slog.Warn("Invalid proxy config from Redis, using file config", "error", err)
		} else {
			*cfg = *savedCfg
			config.ApplyDefaults(cfg)
			configureRuntimeLogging(cfg)
			slog.Debug("Config loaded from Redis")
		}
	}
	slog.Info("Media storage initialized", "directory", cfg.MediaDir)

	lb := loadbalancer.NewWithCacheTTL(s, time.Duration(cfg.LoadBalancerCacheTTL)*time.Second)

	// Connection tracker: use Redis when available
	var accountTracker loadbalancer.ConnTracker
	if redisClient := s.RedisClient(); redisClient != nil {
		redisAccountTracker := loadbalancer.NewRedisConnTracker(redisClient, s.RedisPrefix())
		defer redisAccountTracker.Close()
		accountTracker = redisAccountTracker
		lb.SetConnTracker(accountTracker)
		slog.Debug("Connection tracker initialized", "backend", "redis")
	}

	// Admin sessions are persisted in Redis so a restart — every deploy — no
	// longer signs every operator out of the admin UI. Without a client the
	// in-process store stays in place as the fallback.
	if sessionBackend := auth.NewRedisSessionBackend(s.RedisClient(), s.RedisPrefix()); sessionBackend != nil {
		auth.SetSessionBackend(sessionBackend)
		slog.Debug("Admin sessions persisted", "backend", "redis")
	}

	apiHandler := api.New(s, cfg.AdminUser, cfg.AdminPass, cfg)
	diagnosticStore := debug.NewDiagnosticStore(s.RedisClient(), s.RedisPrefix())
	apiHandler.SetDiagnosticStore(diagnosticStore)
	apiHandler.SetConnectionTracker(accountTracker)
	h := handler.NewWithLoadBalancer(cfg, lb)
	defer h.Close()
	grokHandler := grok.NewHandler(cfg, lb)
	// Gateway-owned Responses compaction seals its summaries with a key derived
	// from the credential key, so the state survives a restart and a request
	// served by another account. Without a key the feature stays off.
	compactionCipher, cipherErr := secureblob.NewCipher(credentialKey)
	if cipherErr != nil {
		slog.Error("Failed to derive the gateway compaction key", "error", cipherErr)
		os.Exit(1)
	}
	grokHandler.SetCompactionCipher(compactionCipher)
	logAnonymousAllowlist(cfg)
	apiHandler.SetConfigChangeHook(func(next *config.Config) {
		configureRuntimeLogging(next)
		h.SetConfig(next)
		grokHandler.SetConfig(next)
	})
	if accountTracker != nil {
		h.SetConnTracker(accountTracker)
		grokHandler.SetConnTracker(accountTracker)
	}

	if redisClient := s.RedisClient(); redisClient != nil {
		auditLogger := audit.NewRedisLogger(redisClient, s.RedisPrefix(), 10000)
		apiHandler.SetAuditHealthReporter(auditLogger.Health)
		h.SetAuditLogger(middleware.ObserveAuditLogger(auditLogger))
		grokHandler.SetAuditLogger(middleware.ObserveAuditLogger(auditLogger))
		// The admin session wrapper journals management changes; wiring the same
		// logger keeps requests and operations in one searchable journal.
		middleware.SetOperationAuditLogger(auditLogger)
		middleware.SetRequestAuditLogger(auditLogger)
		// Client-key billing settles against the same store that took the
		// reservation. Wiring it once keeps every channel's settle path identical.
		middleware.SetAPIKeyBillingStore(s)
		// Per-minute buckets back the operations overview. The trace middleware
		// reports one observation per finished request, so the counters cannot
		// double count an upstream retry.
		opsAggregator := opsagg.New(redisClient, s.RedisPrefix())
		middleware.SetDetailedOutcomeRecorder(opsAggregator.Observe)
		wiredOps = opsAggregator
		apiHandler.SetOpsAggregator(opsAggregator)
		apiHandler.SetRefreshConcurrencyReporter(refresh.Hub.Len)
		// Alert transitions are journalled as system events, which is what makes a
		// failure and its recovery one traceable pair.
		alertRules := alerting.DefaultRules()
		if raw, err := redisClient.Get(context.Background(), s.RedisPrefix()+"ops:alert_rules").Bytes(); err == nil {
			var saved alerting.Rules
			if json.Unmarshal(raw, &saved) == nil && saved.Validate() == nil {
				alertRules = saved
			}
		}
		alertEngine = alerting.NewEngine(alertRules, alerting.AuditRecorder(auditLogger))
		apiHandler.SetAlertEngine(alertEngine)
		wiredAuditLogger = auditLogger
		// One account change, three caches: the pool snapshot, the cached upstream
		// clients and the refresh scheduler's due set. The store announces a change
		// only after it has been persisted, and the bus coalesces bursts by account
		// ID so a multi-field update invalidates each cache once.
		accountBus := accountevents.NewBus()
		defer accountBus.Close()
		accountBus.Subscribe(lb)
		accountBus.Subscribe(h)
		accountBus.Subscribe(refresh.Kick)
		wiredStore = s
		s.SetChangeEmitter(accountChangeEmitter{bus: accountBus})
		slog.Info("Operations aggregation wired",
			"bucket_prefix", s.RedisPrefix()+"ops:agg:",
			"request_recorder", true,
			"alert_engine", true,
			"account_change_bus", "in-process")
		defer auditLogger.Close()
		slog.Debug("Audit logger initialized", "backend", "redis")
	}

	// Every channel's upstream client is built through the provider table.
	h.SetClientFactory(func(acc *store.Account, c *config.Config) handler.UpstreamClient {
		if factory, ok := provider.Get(acc.AccountType); ok {
			if client, ok := factory(acc, c).(handler.UpstreamClient); ok {
				// WorkBuddy rotates its refresh token on every renewal; give the
				// client the store so the rotated credential survives the call.
				if wb, ok := client.(interface {
					SetAccountStore(workbuddy.AccountUpdater)
				}); ok {
					wb.SetAccountStore(s)
				}
				// Qoder also rotates its refresh token upstream; give the client
				// the store so the rotated credential survives the call.
				if qd, ok := client.(interface {
					SetAccountStore(qoder.AccountUpdater)
				}); ok {
					qd.SetAccountStore(s)
				}
				// Cline rotates its refresh token on every renewal as well.
				if cl, ok := client.(interface {
					SetAccountStore(cline.AccountUpdater)
				}); ok {
					cl.SetAccountStore(s)
				}
				return client
			}
		}
		return nil
	})

	// Initialize template renderer
	tmplRenderer, err := template.NewRenderer()
	if err != nil {
		slog.Error("Failed to initialize template renderer", "error", err)
		os.Exit(1)
	}
	slog.Debug("Template renderer initialized")

	// Register routes
	mux := http.NewServeMux()
	limiter := middleware.NewConcurrencyLimiter(cfg.ConcurrencyLimit, time.Duration(cfg.ConcurrencyTimeout)*time.Second)
	updater := registerRoutes(mux, cfg, s, h, grokHandler, apiHandler, limiter, accountTracker, tmplRenderer)
	trustedProxy, err := middleware.TrustedProxyMiddleware(cfg.TrustedProxies)
	if err != nil {
		slog.Error("Invalid trusted proxy configuration", "error", err)
		os.Exit(1)
	}

	// Build server
	server := &http.Server{
		Addr: ":" + cfg.Port,
		Handler: middleware.Chain(
			trustedProxy,
			middleware.SecurityHeaders,
			middleware.TraceMiddleware,
			middleware.Diagnostics(diagnosticStore, apiHandler.DiagnosticsEnabled, func() middleware.DiagnosticBudget {
				current := apiHandler.ConfigSnapshot()
				if current == nil {
					return middleware.DiagnosticBudget{}
				}
				return middleware.DiagnosticBudget{SampleEvery: current.DiagnosticsSampleEvery, MaxConcurrent: current.DiagnosticsMaxConcurrent}
			}),
			middleware.LoggingMiddleware,
		)(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Start background tasks
	ctx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()
	updater.StartBackground(ctx)

	refresh.StartTokenRefreshLoop(ctx, apiHandler.ConfigSnapshot, s, lb)
	modelrefresh.StartCatalogRefreshLoop(ctx, apiHandler.ConfigSnapshot, s)
	// Alert evaluation runs beside the refresh loop: it reads the same metric
	// buckets the overview shows, so an alert and the page never disagree.
	alerting.StartLoop(ctx, wiredOps, s, alertEngine, wiredAuditLogger)
	logReachability("WorkBuddy backend", "workbuddy", workbuddy.DefaultBaseURL,
		func() reachableClient { return workbuddy.NewFromAccount(nil, cfg) })
	logReachability("Qoder control plane", "qoder", qoder.DefaultOpenAPIBaseURL,
		func() reachableClient { return qoder.NewFromAccount(nil, cfg) })
	logReachability("Cline API", "cline", cline.DefaultAPIBase,
		func() reachableClient { return cline.NewFromAccount(nil, cfg) })

	// Graceful shutdown
	idleConnsClosed := make(chan struct{})
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		sig := <-quit
		slog.Info("Received signal, starting graceful shutdown", "signal", sig)

		cancelBackground()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("Server shutdown error", "error", err)
		}
		close(idleConnsClosed)
	}()

	slog.Info("Server running", "port", cfg.Port)
	slog.Info("Admin UI available", "url", "http://localhost:"+cfg.Port+cfg.AdminPath)

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("Server start failed", "error", err)
		os.Exit(1)
	}

	<-idleConnsClosed
	slog.Info("Server shutdown gracefully")
}

// reachableClient is the shape shared by the per-provider startup probes.
type reachableClient interface {
	ProbeReachability(ctx context.Context) error
	Close()
}

// logReachability reports at startup whether this process can reach a provider
// backend. A blocked egress path breaks both the login and every inference
// request, so the cause should be visible in the boot log instead of surfacing
// as a per-request 502.
func logReachability(subject, channel, endpoint string, newClient func() reachableClient) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		client := newClient()
		defer client.Close()
		if err := client.ProbeReachability(ctx); err != nil {
			slog.Warn(subject+" is not reachable; the "+channel+" channel will fail until egress is fixed",
				"endpoint", endpoint, "error", err,
				"hint", "configure HTTP_PROXY/HTTPS_PROXY or the proxy settings in config.json if this host needs one")
			return
		}
		slog.Info(subject+" reachable", "endpoint", endpoint)
	}()
}

// logAnonymousAllowlist states which sources may call the inference routes
// without a key. It is a deviation from the reference implementation, so a
// deployment that uses it should see it in the log rather than infer it.
func logAnonymousAllowlist(cfg *config.Config) {
	if cfg == nil || len(cfg.AnonymousAllowIPs) == 0 {
		return
	}
	list, err := middleware.NewAnonymousAllowlist(cfg.AnonymousAllowIPs)
	if err != nil || list.Empty() {
		slog.Error("anonymous_allow_ips is not usable; every caller must present a key", "error", err)
		return
	}
	slog.Warn("anonymous inference access is allowed for the configured sources; every other caller still needs a key",
		"anonymous_allow_ips", cfg.AnonymousAllowIPs)
}

func configureRuntimeLogging(cfg *config.Config) {
	level := slog.LevelInfo
	if cfg != nil && cfg.DebugEnabled {
		level = slog.LevelDebug
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	logutil.SetVerboseDiagnostics(cfg != nil && cfg.VerboseDiagnosticsEnabled())
}
