// Package api serves the management console: accounts, API keys, models,
// configuration, audit and the operations overview. It is the admin surface, not
// the inference surface — nothing here runs on an inference request path.
//
// The handlers were once one file. They are now split by subject, each file
// holding the handlers and the helpers only that subject needs:
//
//	api.go                 the API struct, its wiring and constructor
//	api_accounts.go        account create/read/update/delete handlers
//	account_output.go      one account as the API renders it, credentials stripped
//	account_identity.go    recognising an account the gateway already has
//	account_verdict.go     what a refresh or a check concludes, and for how long
//	api_keys.go            API key list/read/create/update/delete
//	api_models.go          model list/read
//	api_config.go          config read/save, login/logout, config patching
//	api_export_import.go   account export and import
//	channel_port.go        how this package reaches a channel
//	api_channel_login.go   the browser login every channel shares
//	api_device_login.go    Grok device-authorization flow
//	api_audit.go           audit journal query and filter helpers
//	api_ops.go             operations overview, runtime metrics, channels
//
// Nothing here knows a channel's credential fields, endpoints or allowance
// rules. Each channel implements provider.Admin in its own package, and the
// handlers ask it: adding a channel does not touch this package.
package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"orchids-api/internal/alerting"
	"orchids-api/internal/audit"
	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/opsagg"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

type API struct {
	importMu     sync.Mutex
	configMu     sync.Mutex
	configHookMu sync.RWMutex
	connTracker  loadbalancer.ConnTracker
	store        *store.Store
	adminUser    string
	adminPass    string
	loginLimiter *middleware.RateLimiter
	config       atomic.Pointer[config.Config]
	configHook   func(*config.Config)

	// Account check backoff / storm control
	checkMu          sync.Mutex
	checkInFlight    map[int64]bool
	checkFailCount   map[int64]int
	checkNextAllowed map[int64]time.Time
	checkSem         chan struct{}

	// Device logins hold only a short-lived, in-memory device code and the
	// credential a completed login produced. Each channel keeps its own registry
	// so codes and credentials can never cross authentication flows; the storage
	// and bookkeeping behind them is shared (see deviceLoginRegistry).
	grokLogins      *deviceLoginRegistry[deviceLogin]
	workbuddyLogins *deviceLoginRegistry[browserLogin]
	qoderLogins     *deviceLoginRegistry[browserLogin]
	clineLogins     *deviceLoginRegistry[browserLogin]

	// opsAggregator and alerts back the operations overview. They are optional:
	// a Redis-less deployment simply reports "no sample" instead of failing.
	auditHealth   func() audit.Health
	diagnostics   *debug.DiagnosticStore
	alertRulesMu  sync.Mutex
	opsAggregator *opsagg.Aggregator
	alertEngine   *alerting.Engine
	// refreshConcurrency reports how many accounts are being refreshed right now.
	refreshConcurrency func() int
}

// SetRefreshConcurrencyReporter lets the scheduler expose its in-flight count to
// the overview without the API importing the scheduler.
func (a *API) SetRefreshConcurrencyReporter(reporter func() int) {
	if a == nil {
		return
	}
	a.refreshConcurrency = reporter
}

// SetOpsAggregator wires the per-minute buckets used by the overview endpoints.
func (a *API) SetOpsAggregator(aggregator *opsagg.Aggregator) {
	if a == nil {
		return
	}
	a.opsAggregator = aggregator
}

// SetAlertEngine wires the alert rules evaluated by the overview endpoints.
func (a *API) SetAlertEngine(engine *alerting.Engine) {
	if a == nil {
		return
	}
	a.alertEngine = engine
}

// SetAuditHealthReporter exposes process-local sink health without coupling queries to writes.
func (a *API) SetAuditHealthReporter(reporter func() audit.Health) { a.auditHealth = reporter }

func (a *API) refreshAccountState(ctx context.Context, acc *store.Account) (string, int, error) {
	if acc == nil {
		return "", http.StatusBadRequest, fmt.Errorf("account is nil")
	}

	admin := adminFor(acc.AccountType)
	if admin == nil || admin.Verify == nil {
		return "", http.StatusBadRequest, fmt.Errorf("unsupported account type %q", acc.AccountType)
	}
	return verifyThroughChannel(strings.ToLower(strings.TrimSpace(acc.AccountType)), admin.ErrMissingCredential, func() (string, int, error) {
		return admin.Verify(ctx, provider.Env{Config: a.config.Load(), Store: a.store}, acc)
	})
}

type ExportData struct {
	Version  int             `json:"version"`
	ExportAt time.Time       `json:"export_at"`
	Accounts []store.Account `json:"accounts"`
}

type ImportResult struct {
	Total      int           `json:"total"`
	Imported   int           `json:"imported"`
	Skipped    int           `json:"skipped"`
	Duplicates int           `json:"duplicates"`
	Invalid    int           `json:"invalid"`
	Failed     int           `json:"failed"`
	Issues     []ImportIssue `json:"issues,omitempty"`
}

type ImportIssue struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

func New(s *store.Store, adminUser, adminPass string, cfg *config.Config) *API {
	a := &API{
		store:        s,
		adminUser:    adminUser,
		adminPass:    adminPass,
		loginLimiter: middleware.NewRateLimiter(5, 15*time.Minute),

		checkInFlight:    map[int64]bool{},
		checkFailCount:   map[int64]int{},
		checkNextAllowed: map[int64]time.Time{},
		checkSem:         make(chan struct{}, 2),
		grokLogins:       newDeviceLoginRegistry(identityDeviceLogin, nil, "Grok authorization expired"),
		workbuddyLogins:  newBrowserLoginRegistry(channel.WorkBuddy),
		qoderLogins:      newBrowserLoginRegistry(channel.Qoder),
		clineLogins:      newBrowserLoginRegistry(channel.Cline),
	}
	if cfg != nil {
		a.config.Store(cfg.Clone())
	}
	return a
}

// SetConfigChangeHook registers the runtime components that must adopt a newly
// persisted immutable config snapshot. The hook is invoked after the snapshot
// has been durably stored and atomically published by the API.
func (a *API) SetConfigChangeHook(hook func(*config.Config)) {
	if a == nil {
		return
	}
	a.configHookMu.Lock()
	a.configHook = hook
	a.configHookMu.Unlock()
}

// ConfigSnapshot returns the current immutable runtime configuration. Callers
// must treat the returned value as read-only.
func (a *API) ConfigSnapshot() *config.Config {
	if a == nil {
		return nil
	}
	return a.config.Load()
}

func (a *API) notifyConfigChanged(cfg *config.Config) {
	a.configHookMu.RLock()
	hook := a.configHook
	a.configHookMu.RUnlock()
	if hook != nil {
		hook(cfg)
	}
}
