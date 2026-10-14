package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"orchids-api/internal/billingledger"
	"orchids-api/internal/modelrefresh"
	"orchids-api/internal/refresh"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"orchids-api/internal/accountevents"
	"orchids-api/internal/alerting"
	"orchids-api/internal/api"
	"orchids-api/internal/audit"
	"orchids-api/internal/auth"
	"orchids-api/internal/buildinfo"
	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/grok"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/logutil"
	"orchids-api/internal/middleware"
	"orchids-api/internal/opsagg"
	"orchids-api/internal/provider"
	_ "orchids-api/internal/provider/all" // links every channel into the binary
	"orchids-api/internal/secureblob"
	"orchids-api/internal/selfupdate"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
)

// accountChangeEmitter adapts the store's change description to the notification
// bus. The store publishes only after a successful write; the bus decides what
// kind of change it was.
type accountChangeEmitter struct {
	bus *accountevents.Bus
	// store resolves the after-state of a mutation the emitter was told about.
	store *store.Store
}

// Publish implements store.ChangeEmitter. It resolves the after-state on its own
// goroutine (the store hands over only the id and the previous state), stamps the
// change kind, and hands it to the bus.
func (e accountChangeEmitter) Publish(change store.AccountChange) {
	if e.bus == nil || change.AccountID == 0 {
		return
	}
	var current *store.Account
	if e.store != nil {
		if loaded, err := e.store.GetAccount(context.Background(), change.AccountID); err == nil {
			current = loaded
		}
	}
	e.bus.Publish(accountevents.Change{
		AccountID: change.AccountID,
		Kind:      accountevents.Classify(change.Previous, current),
		Origin:    change.Origin,
	})
}

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

	// Derive the compaction key before opening resources that need cleanup.
	compactionCipher, cipherErr := secureblob.NewCipher(credentialKey)
	if cipherErr != nil {
		slog.Error("Failed to derive the gateway compaction key", "error", cipherErr)
		os.Exit(1)
	}

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
	slog.Info("Runtime state directory initialized", "directory", cfg.MediaDir)

	lb := loadbalancer.NewWithCacheTTL(s, config.LoadBalancerCacheTTL)

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
	grokHandler.SetCompactionCipher(compactionCipher)
	apiHandler.SetConfigChangeHook(func(next *config.Config) {
		configureRuntimeLogging(next)
		h.SetConfig(next)
		grokHandler.SetConfig(next)
	})
	if accountTracker != nil {
		h.SetConnTracker(accountTracker)
		grokHandler.SetConnTracker(accountTracker)
	}

	// These three exist only with Redis. Without it they stay nil, the overview
	// reports "no sample" and the alert loop reports no alerts.
	var (
		// opsAggregator holds the per-minute buckets the overview endpoints and
		// the alert evaluator both read.
		opsAggregator *opsagg.Aggregator
		alertEngine   *alerting.Engine
		// auditJournal is where the background loops record system events.
		auditJournal audit.Logger
	)
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
		billingLedger, err := billingledger.New(s, filepath.Join(filepath.Dir(resolvedConfigPath), "data", "billing-pending"))
		if err != nil {
			slog.Error("Failed to open billing settlement outbox", "error", err)
			os.Exit(1)
		}
		billingCtx, stopBilling := context.WithCancel(context.Background())
		defer stopBilling()
		go billingLedger.Run(billingCtx)
		middleware.SetAPIKeyBillingStore(billingLedger)
		// Per-minute buckets back the operations overview. The trace middleware
		// reports one observation per finished request, so the counters cannot
		// double count an upstream retry.
		opsAggregator = opsagg.New(redisClient, s.RedisPrefix())
		middleware.SetDetailedOutcomeRecorder(opsAggregator.Observe)
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
		auditJournal = auditLogger
		// One account change, three caches: the pool snapshot, the cached upstream
		// clients and the refresh scheduler's due set. The store announces a change
		// only after it has been persisted, and the bus coalesces bursts by account
		// ID so a multi-field update invalidates each cache once.
		accountBus := accountevents.NewBus()
		defer accountBus.Close()
		accountBus.Subscribe(lb)
		accountBus.Subscribe(h)
		accountBus.Subscribe(refresh.Kick)
		s.SetChangeEmitter(accountChangeEmitter{bus: accountBus, store: s})
		slog.Info("Operations aggregation wired",
			"bucket_prefix", s.RedisPrefix()+"ops:agg:",
			"request_recorder", true,
			"alert_engine", true,
			"account_change_bus", "in-process")
		defer auditLogger.Close()
		slog.Debug("Audit logger initialized", "backend", "redis")
	}

	// Initialize template renderer
	tmplRenderer, err := template.NewRenderer()
	if err != nil {
		slog.Error("Failed to initialize template renderer", "error", err)
		os.Exit(1)
	}
	slog.Debug("Template renderer initialized")

	// Register routes
	mux := http.NewServeMux()
	limiter := middleware.NewConcurrencyLimiter(cfg.ConcurrencyLimit, config.InferenceRequestTimeout)
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
	alerting.StartLoop(ctx, opsAggregator, s, alertEngine, auditJournal)
	for _, spec := range provider.All() {
		if spec.Probe != nil {
			logReachability(spec.ID, spec.Probe, cfg)
		}
	}

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

// logReachability reports at startup whether this process can reach a provider
// backend. A blocked egress path breaks both the login and every inference
// request, so the cause should be visible in the boot log instead of surfacing
// as a per-request 502.
func logReachability(id channel.ID, probe *provider.Probe, cfg *config.Config) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if err := probe.Run(ctx, cfg); err != nil {
			slog.Warn(probe.Subject+" is not reachable; the "+string(id)+" channel will fail until egress is fixed",
				"endpoint", probe.Endpoint, "error", err,
				"hint", "configure HTTP_PROXY/HTTPS_PROXY or the proxy settings in config.json if this host needs one")
			return
		}
		slog.Info(probe.Subject+" reachable", "endpoint", probe.Endpoint)
	}()
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
