package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/alerting"
	"orchids-api/internal/audit"
	"orchids-api/internal/auth"
	"orchids-api/internal/channel"
	"orchids-api/internal/cline"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/grok"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/opsagg"
	"orchids-api/internal/qoder"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
	"orchids-api/internal/tokencache"
	"orchids-api/internal/util"
)

type API struct {
	configMu     sync.Mutex
	configHookMu sync.RWMutex
	connTracker  loadbalancer.ConnTracker
	store        *store.Store
	tokenCache   tokencache.Cache
	promptCache  tokencache.PromptCache
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
	workbuddyLogins *deviceLoginRegistry[workbuddyLogin]
	qoderLogins     *deviceLoginRegistry[qoderLoginTransaction]
	clineLogins     *deviceLoginRegistry[clineLoginTransaction]

	// opsAggregator and alerts back the operations overview. They are optional:
	// a Redis-less deployment simply reports "no sample" instead of failing.
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

type auditEventRecord struct {
	ID    string      `json:"id"`
	Event audit.Event `json:"event"`
}

// auditScanCap bounds how many stream entries one query reads before filtering.
// The ledger is time-bounded, so a filtered page must not silently promise the
// whole history: coverage tells the reader what the store actually holds.
const auditScanCap = 2000

// HandleAuditEvents exposes the bounded Redis audit ledger to authenticated
// administrators. Cursor pagination uses Redis Stream IDs; the journal is
// filtered by kind (request/operation/system) plus the fields the log centre
// offers. Credentials never appear: whether they do is enforced at write time by
// audit.SummarizeChange.
// writeAccountCheckBusy tells the caller that a refresh of this account is already
// running, so the click was merged instead of racing a second refresh.
func writeAccountCheckBusy(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    "check_in_progress",
			"message": "this account is already being refreshed; the request was merged",
		},
	})
}

func (a *API) HandleAuditEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a == nil || a.store == nil || a.store.RedisClient() == nil {
		http.Error(w, "audit ledger requires Redis storage", http.StatusServiceUnavailable)
		return
	}
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	filter, err := auditFilterFromQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	maxID := "+"
	if before := strings.TrimSpace(r.URL.Query().Get("before")); before != "" {
		maxID = "(" + before
	} else if !filter.until.IsZero() {
		// Stream ids are time-ordered, so a window that ends in the past starts the
		// scan inside itself instead of walking the newest entries it would filter
		// all out anyway.
		maxID = "(" + strconv.FormatInt(filter.until.UnixMilli()+1, 10)
	}

	scanCount := int64(limit) * 5
	if scanCount > auditScanCap {
		scanCount = auditScanCap
	}
	messages, err := a.store.RedisClient().XRevRangeN(r.Context(), a.store.RedisPrefix()+"audit:log", maxID, "-", scanCount).Result()
	if err != nil {
		http.Error(w, "failed to read audit ledger", http.StatusInternalServerError)
		return
	}
	records := make([]auditEventRecord, 0, len(messages))
	scanned := 0
	for _, message := range messages {
		scanned++
		raw, _ := message.Values["data"].(string)
		var event audit.Event
		if raw == "" || json.Unmarshal([]byte(raw), &event) != nil {
			continue
		}
		if !filter.matches(event) {
			continue
		}
		records = append(records, auditEventRecord{ID: message.ID, Event: event})
		if len(records) >= limit {
			break
		}
	}
	// The cursor must never skip a record: a full page resumes before the last row
	// shown, and a short page that ran out of scan budget resumes before everything
	// that was scanned. An empty cursor means the ledger itself ended.
	nextCursor := ""
	switch {
	case len(records) >= limit:
		nextCursor = records[len(records)-1].ID
	case int64(len(messages)) >= scanCount && len(messages) > 0:
		nextCursor = messages[len(messages)-1].ID
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"data":        records,
		"next_cursor": nextCursor,
		"scanned":     scanned,
		// Filtered: true means the store was scanned to its cap, so an empty page
		// is "nothing matched inside the retained window" — not "never happened".
		"filtered":    scanned >= int(scanCount),
		"scan_cap":    scanCount,
		"coverage":    a.auditCoverage(r.Context()),
		"filter_used": filter.describe(),
	})
}

// auditQueryFilter is the log centre's filter set.
type auditQueryFilter struct {
	kind      string
	channel   string
	status    string
	action    string
	actor     string
	model     string
	outcome   string
	since     time.Time
	until     time.Time
	accountID int64
	apiKeyID  int64
}

// auditOutcomeLabels names the result classes the operations overview counts, so a
// drill-down chip and the chart that opened it use the same words. The ids are the
// ones the log centre's select offers.
var auditOutcomeLabels = map[string]string{
	"failed":          "失败（全部失败类型）",
	"success":         "成功",
	"rate_limited":    "限流 429/529",
	"client_error":    "客户端错误 4xx",
	"server_error":    "上游错误 5xx",
	"stream_error":    "流中断",
	"upstream_auth":   "上游认证失败 401/403",
	"rejected":        "网关拒绝 401/403",
	"quota_exhausted": "额度用尽 402",
}

// auditTimeFormats are the spellings a time filter accepts. The log centre's form
// asks for RFC3339, but an operator copying a timestamp out of a log, or a
// bookmark written by hand, must not silently lose the filter.
var auditTimeFormats = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parseAuditTime reads one boundary of the time range. An empty value means "not
// filtered" and comes back as a zero time; an unparseable value is an error, because
// ignoring it is indistinguishable from a filter that does not work.
func parseAuditTime(name, raw string) (time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range auditTimeFormats {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	// Epoch seconds or milliseconds: what a script or an export hands over.
	if epoch, err := strconv.ParseInt(value, 10, 64); err == nil {
		switch {
		case epoch > 1e12:
			return time.UnixMilli(epoch), nil
		case epoch > 1e9:
			return time.Unix(epoch, 0), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid %s: %q (expected RFC3339, e.g. 2026-09-13T00:00:00Z)", name, value)
}

func auditFilterFromQuery(r *http.Request) (auditQueryFilter, error) {
	query := r.URL.Query()
	parse := func(name string) int64 {
		parsed, err := strconv.ParseInt(strings.TrimSpace(query.Get(name)), 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	}
	since, err := parseAuditTime("since", query.Get("since"))
	if err != nil {
		return auditQueryFilter{}, err
	}
	until, err := parseAuditTime("until", query.Get("until"))
	if err != nil {
		return auditQueryFilter{}, err
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		return auditQueryFilter{}, fmt.Errorf("until %s is before since %s", until.Format(time.RFC3339), since.Format(time.RFC3339))
	}
	return auditQueryFilter{
		kind:      strings.ToLower(strings.TrimSpace(query.Get("kind"))),
		channel:   strings.ToLower(strings.TrimSpace(query.Get("channel"))),
		status:    strings.ToLower(strings.TrimSpace(query.Get("status"))),
		action:    strings.ToLower(strings.TrimSpace(query.Get("action"))),
		actor:     strings.ToLower(strings.TrimSpace(query.Get("actor"))),
		model:     strings.ToLower(strings.TrimSpace(query.Get("model"))),
		outcome:   strings.ToLower(strings.TrimSpace(query.Get("outcome"))),
		since:     since,
		until:     until,
		accountID: parse("account_id"),
		apiKeyID:  parse("api_key_id"),
	}, nil
}

// auditMetadataInt reads an integer the journal kept under metadata. The request
// record's HTTP status and first-token latency live there, not in a column of
// their own.
func auditMetadataInt(event audit.Event, key string) int {
	if event.Metadata == nil {
		return 0
	}
	switch value := event.Metadata[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(value))
		return parsed
	case json.Number:
		parsed, _ := value.Int64()
		return int(parsed)
	}
	return 0
}

// auditOutcomeClass names a record's result the way the operations overview counts
// a request (see opsagg's observeDetails), so "结果 = 限流" in the log centre lists
// the requests the chart counted as rate limited. An operation has no HTTP class:
// it either happened or it did not.
func auditOutcomeClass(event audit.Event) string {
	status := strings.ToLower(strings.TrimSpace(event.Status))
	if event.Kind != audit.KindRequest {
		switch status {
		case "":
			return ""
		case "ok", "success":
			return "success"
		default:
			return "failed"
		}
	}
	httpStatus := auditMetadataInt(event, "http_status")
	switch status {
	case "success", "ok", "stop", "tool_calls", "length", "content_filter", "recovered":
		return "success"
	case "stream_error":
		// The status line was already committed: a class of its own, exactly as the
		// overview records it.
		return "stream_error"
	}
	if status == "" {
		// Older records have no status column; the HTTP status is all there is.
		if httpStatus >= 200 && httpStatus < 300 {
			return "success"
		}
		if httpStatus == 0 {
			return ""
		}
	}
	// Provider finish reasons evolve. A completed 2xx response is successful
	// unless the journal explicitly recorded an error; retain the raw finish
	// reason for the UI's more specific badge/tooltip.
	if httpStatus >= 200 && httpStatus < 300 && status != "error" && status != "failed" {
		return "success"
	}
	switch {
	case httpStatus == 429 || httpStatus == 529:
		return "rate_limited"
	case httpStatus == 402:
		return "quota_exhausted"
	case httpStatus == 401 || httpStatus == 403:
		// A refusal on a provider channel is the provider rejecting our credential;
		// a record with no channel never reached a provider, so the gate refused the
		// caller. The overview separates the two the same way.
		if strings.TrimSpace(event.Channel) == "" {
			return "rejected"
		}
		return "upstream_auth"
	case httpStatus >= 500:
		return "server_error"
	case httpStatus >= 400:
		return "client_error"
	}
	// A failure with no HTTP class (a transport error before any status line).
	return "failed"
}

func auditOutcomeLabel(class string) string {
	if label, ok := auditOutcomeLabels[class]; ok {
		return label
	}
	return class
}

func (f auditQueryFilter) matches(event audit.Event) bool {
	if f.kind != "" && string(event.Kind) != f.kind {
		return false
	}
	if f.channel != "" && !strings.EqualFold(event.Channel, f.channel) {
		return false
	}
	if f.status != "" && !strings.EqualFold(event.Status, f.status) {
		return false
	}
	if f.action != "" && !strings.Contains(strings.ToLower(event.Action), f.action) {
		return false
	}
	if f.actor != "" && !strings.Contains(strings.ToLower(event.Actor), f.actor) {
		return false
	}
	if f.model != "" && !strings.Contains(strings.ToLower(event.Model), f.model) {
		return false
	}
	// The time range is the filter a drill-down always carries: the overview counted
	// one window, so a list that ignores it shows records the chart never saw.
	if !f.since.IsZero() && event.Timestamp.Before(f.since) {
		return false
	}
	if !f.until.IsZero() && event.Timestamp.After(f.until) {
		return false
	}
	if f.outcome != "" {
		class := auditOutcomeClass(event)
		if f.outcome == "failed" {
			if class == "" || class == "success" {
				return false
			}
		} else if class != f.outcome {
			return false
		}
	}
	if f.accountID != 0 && event.AccountID != f.accountID {
		return false
	}
	if f.apiKeyID != 0 && event.APIKeyID != f.apiKeyID {
		return false
	}
	return true
}

func (f auditQueryFilter) describe() map[string]interface{} {
	described := map[string]interface{}{}
	if f.kind != "" {
		described["kind"] = f.kind
	}
	if f.channel != "" {
		described["channel"] = f.channel
	}
	if f.status != "" {
		described["status"] = f.status
	}
	if f.action != "" {
		described["action"] = f.action
	}
	if f.actor != "" {
		described["actor"] = f.actor
	}
	if f.model != "" {
		described["model"] = f.model
	}
	if f.outcome != "" {
		described["outcome"] = f.outcome
		described["outcome_label"] = auditOutcomeLabel(f.outcome)
	}
	if !f.since.IsZero() {
		described["since"] = f.since.UTC().Format(time.RFC3339)
	}
	if !f.until.IsZero() {
		described["until"] = f.until.UTC().Format(time.RFC3339)
	}
	if f.accountID != 0 {
		described["account_id"] = f.accountID
	}
	if f.apiKeyID != 0 {
		described["api_key_id"] = f.apiKeyID
	}
	return described
}

// auditCoverage reports the retained window and the per-journal counts, so the
// UI can state what the numbers actually cover instead of promising a fixed
// retention period.
func (a *API) auditCoverage(ctx context.Context) map[string]interface{} {
	coverage := map[string]interface{}{"entries": 0, "oldest": nil, "newest": nil, "counts": map[string]int{}}
	if a == nil || a.store == nil || a.store.RedisClient() == nil {
		return coverage
	}
	client := a.store.RedisClient()
	key := a.store.RedisPrefix() + "audit:log"

	total, err := client.XLen(ctx, key).Result()
	if err != nil {
		return coverage
	}
	coverage["entries"] = total
	counts := map[string]int{}
	var newest, oldest string
	if entries, err := client.XRevRangeN(ctx, key, "+", "-", 1).Result(); err == nil && len(entries) > 0 {
		newest = entries[0].ID
	}
	if entries, err := client.XRangeN(ctx, key, "-", "+", 1).Result(); err == nil && len(entries) > 0 {
		oldest = entries[0].ID
	}
	coverage["oldest"] = streamIDTime(oldest)
	coverage["newest"] = streamIDTime(newest)

	// Per-journal counts over the retained window. The cap keeps the call bounded
	// on a busy instance; counts are therefore a floor, which the response says.
	if entries, err := client.XRevRangeN(ctx, key, "+", "-", auditScanCap).Result(); err == nil {
		coverage["count_sampled"] = len(entries)
		for _, entry := range entries {
			raw, _ := entry.Values["data"].(string)
			var event audit.Event
			if raw == "" || json.Unmarshal([]byte(raw), &event) != nil {
				continue
			}
			kind := string(event.Kind)
			if kind == "" {
				kind = string(audit.KindRequest)
			}
			counts[kind]++
		}
	}
	coverage["counts"] = counts
	return coverage
}

// streamIDTime converts a Redis Stream ID ("ms-seq") into RFC3339, or null.
func streamIDTime(id string) interface{} {
	millis := strings.SplitN(strings.TrimSpace(id), "-", 2)[0]
	if millis == "" {
		return nil
	}
	parsed, err := strconv.ParseInt(millis, 10, 64)
	if err != nil || parsed <= 0 {
		return nil
	}
	return time.UnixMilli(parsed).UTC().Format(time.RFC3339)
}

const maxDeviceLogins = 10

type deviceLogin struct {
	deviceCode string
	userCode   string
	verifyURI  string
	verifyFull string
	expiresAt  time.Time
	interval   time.Duration
	cancel     context.CancelFunc
	done       chan struct{}
	// configSnapshot pins provider endpoints for the whole transaction. A
	// live config reload must not start a login on one host and poll another.
	configSnapshot *config.Config

	status    string
	message   string
	accountID int64

	// enabled/enabledKnown carry a provider-specific preference captured at
	// start time (currently the WorkBuddy account enabled flag).
	enabled      bool
	enabledKnown bool
}

type deviceLoginResponse struct {
	ID                      string `json:"id"`
	Status                  string `json:"status"`
	UserCode                string `json:"user_code,omitempty"`
	VerificationURI         string `json:"verification_uri,omitempty"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresAt               string `json:"expires_at,omitempty"`
	AccountID               int64  `json:"account_id,omitempty"`
	Message                 string `json:"message,omitempty"`
}

func verifyGrokAccount(ctx context.Context, acc *store.Account, cfg *config.Config, accountStore *store.Store) error {
	if acc == nil {
		return fmt.Errorf("missing grok account")
	}
	// Build CLI OAuth accounts verify against the CLI proxy with a Bearer token.
	if grokAccountIsOAuth(acc) {
		if !grokAccountHasOAuthCredentials(acc) {
			return fmt.Errorf("missing oauth token")
		}
		cliClient := grok.NewCLIClient(cfg)
		cliClient.SetAccountStore(accountStore)
		result := grok.RefreshBuildAccount(ctx, cliClient, acc, grok.BuildRefreshOptions{
			Verify: true, VerifyTimeout: 20 * time.Second,
			Billing: true,
			Models:  true, ModelsTimeout: 15 * time.Second,
		})
		if result.VerifyErr != nil {
			if result.VerifyStatus != "" {
				return fmt.Errorf("%s: %w", result.VerifyStatus, result.VerifyErr)
			}
			return result.VerifyErr
		}
		if result.BillingErr != nil {
			slog.Warn("Grok CLI billing sync failed; leaving quota unavailable", "account_id", acc.ID, "error", result.BillingErr)
		}
		if result.ModelsErr != nil {
			slog.Warn("Grok CLI model catalog sync failed; keeping last catalog", "account_id", acc.ID, "error", result.ModelsErr)
		}
		return nil
	}

	// The website cookie plane was retired: only Build OAuth
	// credentials can be verified now.
	return fmt.Errorf("only Grok Build OAuth accounts are supported")
}

func httpStatusFromAccountStatus(status string) int {
	switch strings.TrimSpace(status) {
	case "401":
		return http.StatusUnauthorized
	case "402", store.AccountStatusQoderQuotaExhausted, store.AccountStatusWorkBuddyQuotaExhausted:
		return http.StatusPaymentRequired
	case "403":
		return http.StatusForbidden
	case "404":
		return http.StatusNotFound
	case "429":
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

func normalizeGrokTokenInput(acc *store.Account) {
	if acc == nil || !strings.EqualFold(acc.AccountType, "grok") {
		return
	}
	acc.CredentialType = "oauth"
	acc.GrokProvider = grok.ProviderBuild
	acc.OAuthAccessToken = strings.TrimSpace(acc.OAuthAccessToken)
	acc.OAuthRefreshToken = strings.TrimSpace(acc.OAuthRefreshToken)
	// Non-Build Grok credentials must never survive an account write.
	acc.Token = ""
	acc.ClientCookie = ""
	acc.RefreshToken = ""
	acc.SessionCookie = ""
	acc.SessionID = ""
	acc.ClientUat = ""
}

// grokAccountIsOAuth reports whether a Grok account is a Build CLI OAuth account.
func grokAccountIsOAuth(acc *store.Account) bool {
	return acc != nil && strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth")
}

// grokAccountHasOAuthCredentials reports whether an OAuth account carries at
// least one usable token after normalization.
func grokAccountHasOAuthCredentials(acc *store.Account) bool {
	if !grokAccountIsOAuth(acc) {
		return false
	}
	return strings.TrimSpace(acc.OAuthAccessToken) != "" || strings.TrimSpace(acc.OAuthRefreshToken) != ""
}

// preserveGrokOAuthCredentials keeps existing OAuth secrets when the admin UI
// submits empty fields (secrets are redacted on read and therefore absent on
// ordinary edit/save).
func preserveGrokOAuthCredentials(acc, existing *store.Account) {
	if acc == nil || existing == nil || !grokAccountIsOAuth(acc) {
		return
	}
	if strings.TrimSpace(acc.OAuthAccessToken) == "" {
		acc.OAuthAccessToken = existing.OAuthAccessToken
	}
	if strings.TrimSpace(acc.OAuthRefreshToken) == "" {
		acc.OAuthRefreshToken = existing.OAuthRefreshToken
	}
	if acc.OAuthExpiresAt.IsZero() && !existing.OAuthExpiresAt.IsZero() {
		acc.OAuthExpiresAt = existing.OAuthExpiresAt
	}
	if strings.TrimSpace(acc.TeamID) == "" {
		acc.TeamID = existing.TeamID
	}
}

// preserveGrokRuntimeStateOnAdminEdit keeps provider-observed Build state out of
// the generic account edit surface. OAuth secrets are preserved separately.
func preserveGrokRuntimeStateOnAdminEdit(acc, existing *store.Account) {
	if acc == nil || existing == nil || !strings.EqualFold(acc.AccountType, "grok") {
		return
	}
	acc.Token = existing.Token
	acc.Subscription = existing.Subscription
	acc.UsageCurrent = existing.UsageCurrent
	acc.UsageTotal = existing.UsageTotal
	acc.UsageLimit = existing.UsageLimit
	acc.StatusCode = existing.StatusCode
	acc.StatusMessage = existing.StatusMessage
	acc.LastAttempt = existing.LastAttempt
	acc.VerifiedAt = existing.VerifiedAt
	acc.QuotaResetAt = existing.QuotaResetAt
	acc.GrokModels = append([]string(nil), existing.GrokModels...)
	acc.GrokModelCatalog = modelcatalog.CloneProfiles(existing.GrokModelCatalog)
	acc.GrokModelsSyncedAt = existing.GrokModelsSyncedAt
	acc.GrokBilling = existing.GrokBilling
	acc.GrokRateLimits = existing.GrokRateLimits
}

type accountOutput struct {
	*store.Account
	// SessionFingerprint is a short digest of the credential the account is
	// authenticated with. It lets the table tell two sessions apart on channels
	// that carry no email, without returning the secret itself.
	SessionFingerprint string `json:"session_fingerprint,omitempty"`
	// Quota holds the provider-specific quota projection. It is merged into every
	// account response so the management table can render 等级/配额 consistently
	// without re-deriving each channel's semantics on the client.
	Quota map[string]interface{} `json:"-"`
}

// MarshalJSON flattens the quota projection into the account object itself.
func (o accountOutput) MarshalJSON() ([]byte, error) {
	merged := map[string]interface{}{}
	if o.Account != nil {
		raw, err := json.Marshal(o.Account)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &merged); err != nil {
			return nil, err
		}
	}
	// The session fingerprint identifies a login on channels that carry no email;
	// it is a digest, never the credential, so it is safe to expose to an
	// authenticated administrator.
	if o.SessionFingerprint != "" {
		merged["session_fingerprint"] = o.SessionFingerprint
	}
	for key, value := range o.Quota {
		merged[key] = value
	}
	// Credentials are write-only. The account API exposes only their presence,
	// including for create, update and refresh responses.
	merged["has_credential"] = o.SessionFingerprint != ""
	for _, field := range []string{"token", "client_cookie", "refresh_token", "session_cookie", "session_id", "client_uat", "oauth_access_token", "oauth_refresh_token", "workbuddy_access_token", "workbuddy_refresh_token", "qoder_access_token", "qoder_refresh_token", "qoder_runtime_info", "qoder_runtime_key", "cline_access_token", "cline_refresh_token", "session_fingerprint"} {
		delete(merged, field)
	}
	if o.Account != nil {
		merged["status_message"] = redactAccountSecrets(o.Account.StatusMessage, o.Account)
	}
	return json.Marshal(merged)
}

// redactAccountSecrets replaces every credential value an account holds with a
// placeholder, so a text field that quotes upstream output cannot publish one.
func redactAccountSecrets(message string, acc *store.Account) string {
	for _, secret := range acc.Secrets() {
		if secret = strings.TrimSpace(secret); secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}

func normalizeAccountOutput(acc *store.Account) *accountOutput {
	return normalizeAccountOutputWithUsage(acc, nil)
}

// normalizeAccountOutputWithUsage renders one account for the management API.
//
// usage carries the tokens this gateway observed per account inside the Free window;
// a nil map means "not measured", which the quota projection reports honestly instead
// of presenting zero usage as a measurement.
func normalizeAccountOutputWithUsage(acc *store.Account, usage map[int64]int64) *accountOutput {
	// The session fingerprint is derived from the live credential before the
	// redaction below clears it, so the operator can still tell two browser
	// logins apart without the session token ever leaving the server.
	sessionFingerprint := accountSessionFingerprint(acc)
	if acc == nil {
		return nil
	}
	// The projection renders a copy: the channel redaction below clears slots on
	// the way out, and the stored record must keep the credential it was given.
	out := *acc
	// The message is redacted with the same list the final render uses. The two
	// used to differ: this one omitted Qoder's access token and runtime pair, and
	// it ran before the channel projection cleared them, so an upstream error that
	// echoed a Qoder token published it in status_message.
	out.StatusMessage = redactAccountSecrets(acc.StatusMessage, acc)
	if strings.EqualFold(out.AccountType, "grok") {
		grok.NormalizeProvider(&out)
		// The tier column must agree with the quota column. A Build Free account
		// has no plan name from the identity endpoint (recorded as "unknown"), yet
		// the same Free inference that produces its quota window already proves it
		// is Free — and only Free. Reporting "未知" there told an operator nothing
		// about an account the gateway had already characterised.
		if verdict := grok.InferFreeProfile(&out); verdict.Inferred {
			switch strings.ToLower(strings.TrimSpace(out.Subscription)) {
			case "", "unknown", "free":
				out.Subscription = "free"
			}
		}
		out.RefreshToken = ""
		out.SessionCookie = ""
		// The administrator explicitly opted in to seeing the short-lived OAuth
		// access token in the authenticated management UI. Never return the
		// durable refresh token through normal account endpoints.
		out.OAuthRefreshToken = ""
	}
	if strings.EqualFold(out.AccountType, "workbuddy") {
		// The durable refresh token never leaves the server; the access token
		// stays visible so the account table can prove a credential exists.
		out = *RedactWorkBuddyOutput(&out)
	}
	if strings.EqualFold(out.AccountType, "qoder") {
		// The durable refresh token and the derived runtime material never leave
		// the server; the access token stays visible so the account table can
		// prove a credential exists.
		out = *RedactQoderOutput(&out)
	}
	if strings.EqualFold(out.AccountType, "cline") {
		// The durable refresh token never leaves the server; the access token
		// stays visible so the account table can prove a credential exists.
		out = *RedactClineOutput(&out)
	}
	return &accountOutput{
		Account:            &out,
		SessionFingerprint: sessionFingerprint,
		Quota:              buildQuotaResponseFieldsWithUsage(&out, usage[out.ID], usage != nil),
	}
}

// normalizeAccountOutputObserved renders one account together with the Free-window
// usage this gateway measured for it, so a single-account response carries the same
// estimate as the list. A failed measurement falls back to the plain projection
// rather than reporting zero usage as if it had been counted.
func (a *API) normalizeAccountOutputObserved(ctx context.Context, acc *store.Account) *accountOutput {
	observed, ok := a.observedTokensByAccount(ctx, time.Now().Add(-grok.FreeBuildUsageWindow))
	if !ok {
		return normalizeAccountOutput(acc)
	}
	return normalizeAccountOutputWithUsage(acc, observed)
}

// observedTokensByAccount sums the tokens the journal recorded for each account
// inside the Free window.
//
// A Build Free allowance is a rolling token window that the upstream only reveals
// once it is exhausted, so the only honest usage figure available to the admin UI is
// what this gateway itself saw. The scan is bounded (auditScanCap newest entries),
// which makes the sum a floor rather than a total: it travels with quota_observed so
// an estimate is never mistaken for a complete count. A failed scan returns ok=false,
// and callers must then leave the usage unmeasured.
func (a *API) observedTokensByAccount(ctx context.Context, since time.Time) (map[int64]int64, bool) {
	if a == nil || a.store == nil || a.store.RedisClient() == nil {
		return nil, false
	}
	entries, err := a.store.RedisClient().XRevRangeN(ctx, a.store.RedisPrefix()+"audit:log", "+", "-", auditScanCap).Result()
	if err != nil {
		return nil, false
	}
	usage := make(map[int64]int64, len(entries))
	for _, entry := range entries {
		event, ok := decodeAuditEvent(entry)
		if !ok || event.AccountID == 0 {
			continue
		}
		if !since.IsZero() && event.Timestamp.Before(since) {
			continue
		}
		tokens := event.TotalTokens
		if tokens <= 0 {
			tokens = event.InputTokens + event.OutputTokens
		}
		if tokens > 0 {
			usage[event.AccountID] += int64(tokens)
		}
	}
	return usage, true
}

// accountSessionFingerprint returns a short, non-reversible identifier of the
// credential an account is authenticated with.
//
// It exists because some channels authenticate with a session token that carries
// no identity at all (there is no email or username to show). The account table
// then had nothing to display but "登录会话已配置", which made two different
// logins look identical. The fingerprint distinguishes them without ever
// exposing the secret: 12 hex characters of a SHA-256 digest, the same shape
// already used for upstream diagnostics.
func accountSessionFingerprint(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(acc.AccountType)) {
	case "grok":
		if strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") {
			return util.Fingerprint(util.FirstNonEmpty(acc.OAuthAccessToken, acc.OAuthRefreshToken))
		}
		return util.Fingerprint(util.FirstNonEmpty(acc.ClientCookie, acc.RefreshToken, acc.Token))
	case "workbuddy":
		creds := resolveWorkBuddyCredentials(acc)
		return util.Fingerprint(util.FirstNonEmpty(creds.AccessToken, creds.RefreshToken))
	case "qoder":
		creds := qoder.ResolveCredentials(acc)
		return util.Fingerprint(util.FirstNonEmpty(creds.RefreshToken, creds.AccessToken))
	case "cline":
		creds := cline.ResolveCredentials(acc)
		return util.Fingerprint(util.FirstNonEmpty(creds.RefreshToken, creds.AccessToken))
	default:
		return ""
	}
}

func normalizedAccountCredentialKey(acc *store.Account) string {
	if acc == nil {
		return ""
	}

	accountType := strings.ToLower(strings.TrimSpace(acc.AccountType))
	var token string

	switch accountType {
	case "grok":
		token = strings.TrimSpace(util.FirstNonEmpty(acc.OAuthRefreshToken, acc.OAuthAccessToken))
	case "workbuddy":
		return WorkBuddyCredentialKey(acc)
	case "qoder":
		return QoderCredentialKey(acc)
	case "cline":
		return ClineCredentialKey(acc)
	default:
		token = strings.TrimSpace(util.FirstNonEmpty(acc.RefreshToken, acc.SessionCookie, acc.ClientCookie, acc.Token))
	}

	if token == "" || accountType == "" {
		return ""
	}
	return accountType + ":" + token
}

func isSupportedAccountType(accountType string) bool {
	return channel.IsSupported(accountType)
}

// validateAccountType rejects an account whose type is missing or unknown.
//
// The create and update surfaces both take an account type from the request
// body, and both have to answer the same two questions before touching the
// store: is a type present, and is it one this gateway serves. Reporting the
// error and writing the response belongs here so the two surfaces cannot drift
// into giving different answers about the same input.
func validateAccountType(w http.ResponseWriter, accountType string) bool {
	if strings.TrimSpace(accountType) == "" {
		http.Error(w, "account_type is required", http.StatusBadRequest)
		return false
	}
	if !isSupportedAccountType(accountType) {
		http.Error(w, "unsupported account type", http.StatusBadRequest)
		return false
	}
	return true
}

func (a *API) findDuplicateAccountByCredential(ctx context.Context, acc *store.Account, excludeID int64) (*store.Account, error) {
	if a == nil || a.store == nil || acc == nil {
		return nil, nil
	}

	key := normalizedAccountCredentialKey(acc)
	identityKey := stableProviderIdentityKey(acc)
	if key == "" && identityKey == "" {
		return nil, nil
	}

	accounts, err := a.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, existing := range accounts {
		if existing == nil || existing.ID == excludeID {
			continue
		}
		if identityKey != "" && stableProviderIdentityKey(existing) == identityKey {
			return existing, nil
		}
		if key != "" && normalizedAccountCredentialKey(existing) == key {
			return existing, nil
		}
	}
	return nil, nil
}

// saveNewAccountUnlessDuplicate stores a freshly authenticated account, or
// returns the row that already carries its credential.
//
// A completed device login and a completed browser login reach the same
// decision — an upstream may hand out a second grant for an account this
// gateway already has, and inserting it would give the scheduler two rows for
// one allowance. The duplicate check and the insert share a single deadline
// because they are one step: leaving it to the caller's context would let a
// login hold a store round-trip open for the whole poll lifetime.
func (a *API) saveNewAccountUnlessDuplicate(ctx context.Context, acc *store.Account) (*store.Account, error) {
	storeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	existing, err := a.findDuplicateAccountByCredential(storeCtx, acc, 0)
	if err == nil && existing == nil {
		err = a.store.CreateAccount(storeCtx, acc)
	}
	return existing, err
}

// stableProviderIdentityKey survives OAuth token rotation. WorkBuddy and Qoder
// issue a new durable token during a fresh login, so token-only deduplication
// would create a second row for the same upstream user and leave the old row
// holding a consumed refresh token.
func stableProviderIdentityKey(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(acc.AccountType)) {
	case "grok":
		if grokAccountIsOAuth(acc) {
			if userID := strings.TrimSpace(acc.UserID); userID != "" {
				return "grok:oauth:user:" + userID
			}
			if email := strings.ToLower(strings.TrimSpace(acc.Email)); email != "" {
				return "grok:oauth:email:" + email
			}
		}
	case "workbuddy":
		if uid := strings.TrimSpace(acc.WorkBuddyUID); uid != "" {
			return "workbuddy:uid:" + uid
		}
	case "qoder":
		if uid := strings.TrimSpace(acc.QoderUserID); uid != "" {
			return "qoder:uid:" + uid
		}
		if machineID := strings.TrimSpace(acc.QoderMachineID); machineID != "" {
			return "qoder:machine:" + machineID
		}
	case "cline":
		if email := strings.ToLower(strings.TrimSpace(acc.ClineEmail)); email != "" {
			return "cline:email:" + email
		}
	}
	return ""
}

func duplicateAccountError(existing *store.Account) error {
	if existing == nil {
		return fmt.Errorf("duplicate account token")
	}
	accountType := strings.TrimSpace(existing.AccountType)
	if accountType == "" {
		accountType = "account"
	}
	return fmt.Errorf("duplicate %s token already exists on account #%d", accountType, existing.ID)
}

func buildQuotaResponseFields(acc *store.Account) map[string]interface{} {
	return buildQuotaResponseFieldsWithUsage(acc, 0, false)
}

// applyQuotaProvenance records where a quota number came from and how far it should
// be trusted, next to the number itself.
//
// Without it a table can only say "未知", which conflates three different facts — a
// paid account whose numeric window upstream does not publish, a Free account whose
// window has to be estimated, and an account that was never synced. quota_type is
// paid/free/unknown, quota_source names the signal, quota_confidence is
// confirmed/observed/estimated, and quota_limit_known is false whenever the limit is
// an estimate that the upstream has not confirmed.
func applyQuotaProvenance(fields map[string]interface{}, quotaType, source, confidence, note string, limitKnown, observed bool) {
	fields["quota_type"] = quotaType
	fields["quota_source"] = source
	fields["quota_confidence"] = confidence
	fields["quota_limit_known"] = limitKnown
	fields["quota_observed"] = observed
	if note != "" {
		fields["quota_note"] = note
	}
}

// buildQuotaResponseFieldsWithUsage projects an account's allowance. observedTokens
// is the usage this gateway measured inside grok.FreeBuildUsageWindow and
// usageObserved says whether that measurement actually ran; both are used only by the
// Free estimate, which must never present unmeasured usage as if it were measured.
func buildQuotaResponseFieldsWithUsage(acc *store.Account, observedTokens int64, usageObserved bool) map[string]interface{} {
	fields := map[string]interface{}{
		"quota_limit":     0.0,
		"quota_used":      0.0,
		"quota_remaining": 0.0,
		"quota_mode":      "remaining",
		"quota_unit":      "credits",
		"quota_supported": true,
	}
	applyQuotaProvenance(fields, "unknown", "unknown", "", "", false, false)
	if acc == nil {
		return fields
	}

	limit := acc.UsageLimit
	current := acc.UsageCurrent
	if limit < 0 {
		limit = 0
	}
	if current < 0 {
		current = 0
	}

	projectQuotaFields(fields, acc, limit, current, observedTokens, usageObserved)
	return fields
}

func (a *API) refreshAccountState(ctx context.Context, acc *store.Account) (string, int, error) {
	if acc == nil {
		return "", http.StatusBadRequest, fmt.Errorf("account is nil")
	}

	refresher := accountRefreshers[strings.ToLower(strings.TrimSpace(acc.AccountType))]
	if refresher == nil {
		return "", http.StatusBadRequest, fmt.Errorf("unsupported account type %q", acc.AccountType)
	}
	return refresher(a, ctx, acc)
}

type ExportData struct {
	Version  int             `json:"version"`
	ExportAt time.Time       `json:"export_at"`
	Accounts []store.Account `json:"accounts"`
}

type ImportResult struct {
	Total    int `json:"total"`
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
}

type CreateKeyResponse struct {
	ID            int64      `json:"id"`
	Key           string     `json:"key"`
	Name          string     `json:"name"`
	KeyPrefix     string     `json:"key_prefix"`
	KeySuffix     string     `json:"key_suffix"`
	Enabled       bool       `json:"enabled"`
	AllowedModels []string   `json:"allowed_models,omitempty"`
	RPMLimit      int        `json:"rpm_limit,omitempty"`
	MaxConcurrent int        `json:"max_concurrent,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	// BillingLimitUSDTicks is the spending cap in USD ticks
	// (1 USD = 10,000,000,000 ticks); zero means unlimited.
	BillingLimitUSDTicks int64 `json:"billing_limit_usd_ticks,omitempty"`
}

// maxApiKeyBillingLimitUSDTicks caps an admin-supplied billing limit at
// 9,000,000,000,000,000 ticks (900,000 USD), which keeps the sum of live holds
// and settled usage comfortably inside int64.
const maxApiKeyBillingLimitUSDTicks int64 = 9_000_000_000_000_000

// maxApiKeyBillingPeriodDays bounds the rollover window: a decade is plenty and
// keeps the arithmetic on the stored period start sane.
const maxApiKeyBillingPeriodDays = 3650

type UpdateKeyRequest struct {
	Enabled              *bool           `json:"enabled"`
	AllowedModels        *[]string       `json:"allowed_models"`
	RPMLimit             *int            `json:"rpm_limit"`
	MaxConcurrent        *int            `json:"max_concurrent"`
	ExpiresAt            json.RawMessage `json:"expires_at"`
	BillingLimitUSDTicks *int64          `json:"billing_limit_usd_ticks"`
	BillingPeriodDays    *int            `json:"billing_period_days"`
}

func normalizeAllowedModels(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	normalized := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" {
			continue
		}
		if model == "*" {
			return []string{"*"}
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		normalized = append(normalized, model)
	}
	return normalized
}

func parseOptionalExpiry(raw json.RawMessage) (*time.Time, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var expiresAt time.Time
	if err := json.Unmarshal(raw, &expiresAt); err != nil {
		return nil, fmt.Errorf("expires_at must be an RFC3339 timestamp or null")
	}
	expiresAt = expiresAt.UTC()
	return &expiresAt, nil
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
		workbuddyLogins: newDeviceLoginRegistry(
			func(login *workbuddyLogin) *deviceLogin { return &login.deviceLogin }, nil,
			"WorkBuddy authorization expired"),
		qoderLogins: newDeviceLoginRegistry(
			func(login *qoderLoginTransaction) *deviceLogin { return &login.deviceLogin },
			deviceLoginReadyWithoutCode, "Qoder authorization expired"),
		clineLogins: newDeviceLoginRegistry(
			func(login *clineLoginTransaction) *deviceLogin { return &login.deviceLogin }, nil,
			"Cline authorization expired"),
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

func (a *API) SetPromptCache(cache tokencache.PromptCache) {
	a.promptCache = cache
}

func (a *API) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := middleware.ClientIP(r)
	if a.loginLimiter != nil && !a.loginLimiter.Allow(ip) {
		http.Error(w, "Too many login attempts, try again later", http.StatusTooManyRequests)
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	adminUser := a.adminUser
	adminPass := a.adminPass
	if cfg := a.config.Load(); cfg != nil {
		adminUser = cfg.AdminUser
		adminPass = cfg.AdminPass
	}

	if !util.SecureCompare(req.Username, adminUser) || !util.SecureCompare(req.Password, adminPass) {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	token, err := auth.GenerateSessionToken()
	if err != nil {
		slog.Error("Failed to generate session token", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// NOTE: Do not mark cookies as Secure when served over plain HTTP,
	// otherwise browsers will drop the cookie and the Admin UI will appear unable to log in.
	// When behind a TLS-terminating proxy, honor X-Forwarded-Proto.
	isHTTPS := r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 7,
	})

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (a *API) HandleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session_token")
	if err == nil {
		auth.InvalidateSessionToken(cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (a *API) HandleConfig(w http.ResponseWriter, r *http.Request) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(a.config.Load())
	case http.MethodPost:
		// Copy current config, decode into copy, then atomically store
		current := a.config.Load()
		newCfg := current.Clone()
		if err := json.NewDecoder(r.Body).Decode(newCfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := a.persistConfig(r.Context(), current, newCfg); err != nil {
			http.Error(w, "Failed to save config: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(newCfg)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleConfigList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	data, err := configPayload(a.config.Load())
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 1,
			"msg":  "获取配置失败: " + err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"code": 0,
		"data": data,
	})
}

func (a *API) HandleConfigSave(w http.ResponseWriter, r *http.Request) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	current := a.config.Load()
	newCfg, err := buildConfigFromPatch(r, current)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 1,
			"msg":  "parse request failed: " + err.Error(),
		})
		return
	}
	if err := a.persistConfig(r.Context(), current, newCfg); err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 1,
			"msg":  "save config failed: " + err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"code": 0,
		"msg":  "success",
	})
}

func (a *API) HandleAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		accounts, err := a.store.ListAccounts(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if accounts == nil {
			accounts = []*store.Account{}
		}
		// The Free estimate needs the usage this gateway observed; the scan is one
		// bounded read shared by the whole page, and it is skipped entirely when there
		// is nothing to describe.
		var observed map[int64]int64
		if len(accounts) > 0 {
			if measured, ok := a.observedTokensByAccount(r.Context(), time.Now().Add(-grok.FreeBuildUsageWindow)); ok {
				observed = measured
			}
		}
		normalized := make([]*accountOutput, 0, len(accounts))
		for _, acc := range accounts {
			if acc == nil {
				continue
			}
			normalized = append(normalized, normalizeAccountOutputWithUsage(acc, observed))
		}
		json.NewEncoder(w).Encode(normalized)

	case http.MethodPost:
		var acc store.Account
		if err := json.NewDecoder(r.Body).Decode(&acc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		acc.AccountType = strings.ToLower(strings.TrimSpace(acc.AccountType))
		if !validateAccountType(w, acc.AccountType) {
			return
		}
		if strings.EqualFold(acc.AccountType, "grok") {
			normalizeGrokTokenInput(&acc)
			if !grokAccountIsOAuth(&acc) {
				http.Error(w, "Grok accounts must be added through the Build OAuth device login (/api/grok/device-auth)", http.StatusBadRequest)
				return
			}
			if grokAccountIsOAuth(&acc) && !grokAccountHasOAuthCredentials(&acc) {
				http.Error(w, "missing oauth token", http.StatusBadRequest)
				return
			}
		} else if strings.EqualFold(acc.AccountType, "workbuddy") {
			if !NormalizeWorkBuddyCredentials(&acc) {
				http.Error(w, "missing WorkBuddy credential: paste the access token or refresh token from the WorkBuddy desktop session", http.StatusBadRequest)
				return
			}
		} else if strings.EqualFold(acc.AccountType, "qoder") {
			// Qoder is OAuth-only: an account is created by the browser device
			// flow (/api/qoder/login), never by pasting a personal access token.
			http.Error(w, "Qoder accounts must be added using the official browser login (/api/qoder/login)", http.StatusBadRequest)
			return
		} else if strings.EqualFold(acc.AccountType, "cline") {
			// Cline is OAuth-only for the same reason: the WorkOS device grant is
			// the only way to obtain the credential.
			http.Error(w, "Cline accounts must be added using the official browser login (/api/cline/login)", http.StatusBadRequest)
			return
		}
		if existing, err := a.findDuplicateAccountByCredential(r.Context(), &acc, 0); err != nil {
			slog.Error("Failed to detect duplicate account token", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if existing != nil {
			http.Error(w, duplicateAccountError(existing).Error(), http.StatusConflict)
			return
		}

		if err := a.store.CreateAccount(r.Context(), &acc); err != nil {
			slog.Error("Failed to create account", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if acc.Enabled {
			if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Account-Sync")), "async") {
				a.syncAccountAfterCreate(acc)
			} else {
				syncCtx, syncCancel := context.WithTimeout(r.Context(), 25*time.Second)
				accountStatus, _, syncErr := a.refreshAccountState(syncCtx, &acc)
				syncCancel()
				if syncErr != nil {
					slog.Warn("Initial account sync failed", "account_id", acc.ID, "type", acc.AccountType, "error", syncErr)
					if accountStatus != "" {
						acc.StatusCode = accountStatus
						acc.StatusMessage = strings.TrimSpace(syncErr.Error())
						acc.LastAttempt = time.Now()
						acc.VerifiedAt = acc.LastAttempt
					}
				} else {
					applySuccessfulAccountRefreshStatus(&acc, accountStatus)
				}
				// A credential the upstream definitively rejects must not be
				// persisted as a healthy account: it would sit in the pool looking
				// 正常 while every request routed to it fails.
				if acc.StatusCode == "401" {
					if deleteErr := a.store.DeleteAccount(r.Context(), acc.ID); deleteErr != nil {
						slog.Error("Failed to roll back rejected account", "account_id", acc.ID, "type", acc.AccountType, "error", deleteErr)
					} else {
						slog.Warn("Rejected account was not saved (upstream refused the credential)",
							"account_id", acc.ID, "type", acc.AccountType, "reason", acc.StatusMessage)
					}
					apperrors.New("authentication_error",
						"account was rejected by the upstream and was not saved: "+strings.TrimSpace(acc.StatusMessage),
						http.StatusUnauthorized).WriteResponse(w)
					return
				}
				if updateErr := a.store.UpdateAccount(r.Context(), &acc); updateErr != nil {
					slog.Warn("Failed to persist initial account sync", "account_id", acc.ID, "type", acc.AccountType, "error", updateErr)
				}
			}
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(normalizeAccountOutput(&acc))

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func finishDeviceLogin(login *deviceLogin, status, message string, accountID int64) {
	if login == nil {
		return
	}
	login.deviceCode = ""
	login.userCode = ""
	login.verifyURI = ""
	login.verifyFull = ""
	login.status = status
	login.message = message
	login.accountID = accountID
	if login.cancel != nil {
		login.cancel()
	}
}

func newDeviceLoginResponse(id string, login *deviceLogin) deviceLoginResponse {
	response := deviceLoginResponse{ID: id}
	if login == nil {
		return response
	}
	response.Status = login.status
	response.Message = login.message
	response.AccountID = login.accountID
	if login.status == "pending" {
		response.UserCode = login.userCode
		response.VerificationURI = login.verifyURI
		response.VerificationURIComplete = login.verifyFull
		response.ExpiresAt = login.expiresAt.UTC().Format(time.RFC3339)
	}
	return response
}

func newDeviceLoginID() (string, error) {
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// HandleGrokDeviceAuthorization starts and observes the official xAI Grok
// Build CLI device-authorization flow. It accepts no files, browser cookies,
// passwords, or user-supplied tokens; device codes remain server-side only.
func (a *API) HandleGrokDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/grok/device-auth"), "/")
	switch {
	case r.Method == http.MethodPost && path == "":
		a.startGrokDeviceAuthorization(w, r)
	case r.Method == http.MethodGet && path != "":
		a.getGrokDeviceAuthorization(w, path)
	case r.Method == http.MethodDelete && path != "":
		a.cancelGrokDeviceAuthorization(w, path)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) startGrokDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.store == nil {
		http.Error(w, "account store is not configured", http.StatusServiceUnavailable)
		return
	}
	a.grokLogins.cleanup(time.Now())
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	authenticator := grok.NewDeviceAuthenticator(a.config.Load())
	details, err := authenticator.Start(ctx)
	if err != nil {
		slog.Warn("Grok device authorization could not be started", "error", err)
		http.Error(w, "failed to start Grok device authorization", http.StatusBadGateway)
		return
	}
	id, err := newDeviceLoginID()
	if err != nil {
		http.Error(w, "failed to create login transaction", http.StatusInternalServerError)
		return
	}
	pollContext, pollCancel := context.WithCancel(context.Background())
	login := &deviceLogin{
		deviceCode: details.DeviceCode,
		userCode:   details.UserCode,
		verifyURI:  details.VerificationURI,
		verifyFull: details.VerificationURIComplete,
		expiresAt:  time.Now().Add(time.Duration(details.ExpiresIn) * time.Second),
		interval:   time.Duration(details.Interval) * time.Second,
		cancel:     pollCancel,
		status:     "pending",
	}
	if !a.grokLogins.admit(id, login) {
		pollCancel()
		http.Error(w, "too many pending Grok device logins", http.StatusTooManyRequests)
		return
	}
	go a.pollGrokDeviceAuthorization(pollContext, id, authenticator)
	json.NewEncoder(w).Encode(newDeviceLoginResponse(id, login))
}

func (a *API) getGrokDeviceAuthorization(w http.ResponseWriter, id string) {
	a.grokLogins.cleanup(time.Now())
	response, ok := a.grokLogins.response(id)
	if !ok {
		http.Error(w, "Grok device login not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(response)
}

func (a *API) cancelGrokDeviceAuthorization(w http.ResponseWriter, id string) {
	if _, ok := a.grokLogins.cancel(id, "Grok authorization cancelled"); !ok {
		http.Error(w, "Grok device login not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) pollGrokDeviceAuthorization(ctx context.Context, id string, authenticator *grok.DeviceAuthenticator) {
	for {
		login, ok := a.grokLogins.pollable(id)
		if !ok {
			return
		}
		if time.Now().After(login.expiresAt) {
			a.grokLogins.finish(id, "expired", "Grok authorization expired", 0)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(login.interval):
		}
		requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		accessToken, refreshToken, identityToken, expiresAt, err := authenticator.Exchange(requestCtx, login.deviceCode)
		cancel()
		if err != nil {
			if slowDown, pending := grok.IsDeviceAuthorizationPending(err); pending {
				if slowDown {
					a.grokLogins.update(id, func(login *deviceLogin) {
						if login != nil && login.status == "pending" {
							login.interval += 5 * time.Second
						}
					})
				}
				continue
			}
			slog.Warn("Grok device authorization failed", "login_id", id, "error", err)
			a.grokLogins.finish(id, "failed", "Grok authorization failed", 0)
			return
		}
		acc := &store.Account{
			Name:              "grok-device-login",
			AccountType:       "grok",
			CredentialType:    "oauth",
			OAuthAccessToken:  accessToken,
			OAuthRefreshToken: refreshToken,
			OAuthExpiresAt:    expiresAt,
			AgentMode:         "grok-build-0.1",
			Weight:            1,
			Enabled:           true,
		}
		grok.ApplyCLIOAuthIdentity(acc)
		grok.ApplyCLIOAuthIdentityToken(acc, identityToken)
		normalizeGrokTokenInput(acc)
		existing, err := a.saveNewAccountUnlessDuplicate(ctx, acc)
		if err != nil {
			slog.Warn("Grok device authorization could not save account", "login_id", id, "error", err)
			a.grokLogins.finish(id, "failed", "Grok authorization succeeded but account could not be saved", 0)
			return
		}
		if existing != nil {
			// A fresh device grant may rotate the durable refresh token. Update the
			// matching xAI identity in place and enrich legacy generic rows with the
			// email obtained from id_token, while retaining operator settings and
			// accumulated runtime state already held on the row.
			existing.OAuthAccessToken = acc.OAuthAccessToken
			existing.OAuthRefreshToken = acc.OAuthRefreshToken
			existing.OAuthExpiresAt = acc.OAuthExpiresAt
			existing.CredentialType = "oauth"
			existing.GrokProvider = grok.ProviderBuild
			existing.AgentMode = acc.AgentMode
			if acc.UserID != "" {
				existing.UserID = acc.UserID
			}
			if acc.Email != "" {
				existing.Email = acc.Email
				if existing.Name == "" || strings.EqualFold(existing.Name, "grok-device-login") {
					existing.Name = acc.Email
				}
			}
			if acc.TeamID != "" {
				existing.TeamID = acc.TeamID
			}
			existing.StatusCode = ""
			existing.StatusMessage = ""
			existing.LastAttempt = time.Time{}
			existing.ClearVerifiedAt = true
			updateCtx, updateCancel := context.WithTimeout(ctx, 20*time.Second)
			if err := a.store.UpdateAccount(updateCtx, existing); err != nil {
				updateCancel()
				slog.Warn("Grok device authorization could not update account", "login_id", id, "account_id", existing.ID, "error", err)
				a.grokLogins.finish(id, "failed", "Grok authorization succeeded but account could not be updated", 0)
				return
			}
			updateCancel()
			a.grokLogins.finish(id, "complete", "Grok account credentials refreshed", existing.ID)
			return
		}
		a.grokLogins.finish(id, "complete", "Grok account added", acc.ID)
		a.syncAccountAfterCreate(*acc)
		return
	}
}

func (a *API) HandleAccountByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	path := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(path, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	account, err := a.store.GetAccount(r.Context(), id)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	isCheck := len(parts) > 1 && parts[1] == "check"
	isUsage := len(parts) > 1 && parts[1] == "usage"
	if len(parts) > 2 || (len(parts) > 1 && !(isCheck || isUsage)) {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if isUsage {
			resp := map[string]interface{}{
				"account_id":     account.ID,
				"name":           account.Name,
				"account_type":   account.AccountType,
				"subscription":   account.Subscription,
				"usage_current":  account.UsageCurrent,
				"usage_limit":    account.UsageLimit,
				"usage_total":    account.UsageTotal,
				"quota_reset_at": account.QuotaResetAt,
			}
			for k, v := range buildQuotaResponseFields(account) {
				resp[k] = v
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		if isCheck {
			// Storm control / backoff: only allow a small number of concurrent checks,
			// and apply exponential backoff per account on failures.
			now := time.Now()
			a.checkMu.Lock()
			if a.checkInFlight[id] {
				a.checkMu.Unlock()
				http.Error(w, "account check already in progress", http.StatusTooManyRequests)
				return
			}
			if next, ok := a.checkNextAllowed[id]; ok && !next.IsZero() && now.Before(next) {
				retryAfter := int(next.Sub(now).Seconds())
				if retryAfter < 1 {
					retryAfter = 1
				}
				a.checkMu.Unlock()
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				http.Error(w, "account check backoff", http.StatusTooManyRequests)
				return
			}
			a.checkInFlight[id] = true
			a.checkMu.Unlock()
			defer func() {
				a.checkMu.Lock()
				delete(a.checkInFlight, id)
				a.checkMu.Unlock()
			}()

			// global concurrency limit
			a.checkSem <- struct{}{}
			defer func() { <-a.checkSem }()

			acc := account
			checkOK := false
			checkErrStatus := ""
			defer func() {
				a.checkMu.Lock()
				defer a.checkMu.Unlock()
				if checkOK {
					a.checkFailCount[id] = 0
					a.checkNextAllowed[id] = time.Now().Add(3 * time.Second)
					return
				}
				fails := a.checkFailCount[id] + 1
				a.checkFailCount[id] = fails
				d := time.Duration(1<<min(fails, 8)) * time.Second
				// For CF/rate-limit style failures, start with a bigger cooldown.
				if checkErrStatus == "403" || checkErrStatus == "429" {
					if d < 60*time.Second {
						d = 60 * time.Second
					}
				}
				if d > 10*time.Minute {
					d = 10 * time.Minute
				}
				a.checkNextAllowed[id] = time.Now().Add(d)
			}()

			// The manual check takes the same process-wide lease as the background
			// scheduler: two refreshes of one account must never run at once, or the
			// slower writer would persist an older snapshot over a newer verdict.
			var accountStatus string
			var httpStatus int
			var refreshErr error
			if !refreshqueue.WithLease(acc.ID, func() {
				accountStatus, httpStatus, refreshErr = a.refreshAccountState(r.Context(), acc)
			}) {
				slog.Info("Account check skipped: a refresh of this account is already running", "account_id", acc.ID)
				checkErrStatus = ""
				a.checkMu.Lock()
				a.checkInFlight[id] = false
				a.checkMu.Unlock()
				writeAccountCheckBusy(w)
				return
			}
			if refreshErr != nil {
				checkErrStatus = accountStatus
				if accountStatus != "" {
					acc.StatusCode = accountStatus
					// The reason matters: a bare "401" cannot tell an operator
					// whether the credential was retired upstream or the record
					// lost it.
					acc.StatusMessage = strings.TrimSpace(refreshErr.Error())
					acc.LastAttempt = time.Now()
					acc.VerifiedAt = acc.LastAttempt
					if updateErr := a.store.UpdateAccount(r.Context(), acc); updateErr != nil {
						slog.Warn("Failed to persist account refresh status", "account_id", acc.ID, "error", updateErr)
					}
				}
				if httpStatus == 0 {
					httpStatus = http.StatusBadRequest
				}
				http.Error(w, refreshErr.Error(), httpStatus)
				return
			}

			// 刷新/验证成功后清理账号状态
			applySuccessfulAccountRefreshStatus(acc, accountStatus)
			checkOK = true

			if err := a.store.UpdateAccount(r.Context(), acc); err != nil {
				http.Error(w, "Failed to save checked account: "+err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(a.normalizeAccountOutputObserved(r.Context(), acc))
			return
		}
		json.NewEncoder(w).Encode(a.normalizeAccountOutputObserved(r.Context(), account))

	case http.MethodPut:
		existing := account

		var acc store.Account
		if err := json.NewDecoder(r.Body).Decode(&acc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		acc.ID = id
		if strings.TrimSpace(acc.AccountType) == "" {
			acc.AccountType = existing.AccountType
		}
		acc.AccountType = strings.ToLower(strings.TrimSpace(acc.AccountType))
		if !validateAccountType(w, acc.AccountType) {
			return
		}
		if strings.EqualFold(acc.AccountType, "grok") {
			normalizeGrokTokenInput(&acc)
			// Admin UI redacts OAuth secrets on read; empty inbound fields mean
			// "keep existing", not "clear credentials".
			preserveGrokOAuthCredentials(&acc, existing)
			preserveGrokRuntimeStateOnAdminEdit(&acc, existing)
			if grokAccountIsOAuth(&acc) && !grokAccountHasOAuthCredentials(&acc) {
				http.Error(w, "missing oauth token", http.StatusBadRequest)
				return
			}
		} else if strings.EqualFold(acc.AccountType, "workbuddy") {
			// The read path redacts the refresh token, so an ordinary edit
			// arrives without it; keep the stored credential unless a new one
			// was actually submitted.
			submitted := resolveWorkBuddyCredentials(&acc)
			PreserveWorkBuddyCredentialsOnEdit(&acc, existing)
			if resolveWorkBuddyCredentials(&acc).RefreshToken == "" && resolveWorkBuddyCredentials(&acc).AccessToken == "" {
				http.Error(w, "missing WorkBuddy credential", http.StatusBadRequest)
				return
			}
			NormalizeWorkBuddyCredentials(&acc)
			existingCreds := resolveWorkBuddyCredentials(existing)
			acc.ReplaceWorkBuddyCredentials = submitted.HasCredential() &&
				(submitted.AccessToken != existingCreds.AccessToken || submitted.RefreshToken != existingCreds.RefreshToken)
			if acc.ReplaceWorkBuddyCredentials {
				acc.ClearVerifiedAt = true
			}
		} else if strings.EqualFold(acc.AccountType, "qoder") {
			submitted := qoder.ResolveCredentials(&acc)
			submittedMachineID := strings.TrimSpace(acc.QoderMachineID)
			PreserveQoderCredentialsOnEdit(&acc, existing)
			if !NormalizeQoderCredentials(&acc) {
				http.Error(w, "missing Qoder credential: sign in again with the browser login", http.StatusBadRequest)
				return
			}
			if strings.TrimSpace(acc.QoderMachineID) == "" {
				http.Error(w, "missing Qoder device identity: sign in again", http.StatusBadRequest)
				return
			}
			existingCreds := qoder.ResolveCredentials(existing)
			acc.ReplaceQoderCredentials = submitted.HasCredential() &&
				(submitted.AccessToken != existingCreds.AccessToken ||
					submitted.RefreshToken != existingCreds.RefreshToken ||
					(submittedMachineID != "" && submittedMachineID != strings.TrimSpace(existing.QoderMachineID)))
			if acc.ReplaceQoderCredentials {
				acc.ClearVerifiedAt = true
			}
		} else if strings.EqualFold(acc.AccountType, "cline") {
			// The read path redacts the refresh token, so an ordinary edit
			// arrives without it; keep the stored credential unless a new one
			// was actually submitted.
			submitted := cline.ResolveCredentials(&acc)
			PreserveClineCredentialsOnEdit(&acc, existing)
			if !NormalizeClineCredentials(&acc) {
				http.Error(w, "missing Cline credential: sign in again with the browser login", http.StatusBadRequest)
				return
			}
			existingCreds := cline.ResolveCredentials(existing)
			acc.ReplaceClineCredentials = submitted.HasCredential() &&
				(submitted.AccessToken != existingCreds.AccessToken ||
					submitted.RefreshToken != existingCreds.RefreshToken)
			if acc.ReplaceClineCredentials {
				acc.ClearVerifiedAt = true
			}
		}

		if acc.SessionID == "" {
			acc.SessionID = existing.SessionID
		}
		if acc.SessionCookie == "" {
			acc.SessionCookie = existing.SessionCookie
		}
		if acc.ClientUat == "" {
			acc.ClientUat = existing.ClientUat
		}
		if acc.UserID == "" {
			acc.UserID = existing.UserID
		}
		if acc.Email == "" {
			acc.Email = existing.Email
		}
		if duplicate, err := a.findDuplicateAccountByCredential(r.Context(), &acc, id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if duplicate != nil {
			http.Error(w, duplicateAccountError(duplicate).Error(), http.StatusConflict)
			return
		}

		if err := a.store.UpdateAccount(r.Context(), &acc); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(normalizeAccountOutput(&acc))

	case http.MethodDelete:
		if err := a.store.DeleteAccount(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleGrokAvailability(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	counts := map[string]int{grok.ProviderBuild: 0}
	accounts, err := a.store.ListAccounts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, acc := range accounts {
		if acc == nil || !acc.Enabled {
			continue
		}
		if provider := grok.ProviderForAccount(acc); provider != "" {
			if _, ok := counts[provider]; ok {
				counts[provider]++
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"counts": counts})
}

func (a *API) HandleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	accounts, err := a.store.ListAccounts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	exportData := ExportData{
		Version:  1,
		ExportAt: time.Now(),
		Accounts: make([]store.Account, 0, len(accounts)),
	}
	for _, acc := range accounts {
		normalized := *normalizeAccountOutput(acc).Account
		// Restore the durable credential the read path hides, then drop anything
		// that belongs to another channel. An export that drops a channel's
		// durable credential is unusable on re-import: both WorkBuddy and Qoder
		// rotate a refresh token that is the only way to renew, so an account
		// restored from such a file works until its access token expires and then
		// cannot recover.
		restoreExportCredentials(&normalized, acc)
		redactForeignCredentials(&normalized)
		normalized.ID = 0
		normalized.RequestCount = 0
		exportData.Accounts = append(exportData.Accounts, normalized)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=accounts_export.json")
	json.NewEncoder(w).Encode(exportData)
}

// restoreExportCredentials puts back the credential a channel needs to be usable
// after import.
//
// normalizeAccountOutput hides these for list and query responses, so the export
// has to restore them explicitly. Everything restored here is the channel's own
// credential; a value that belongs to a different channel is cleared right after
// by redactForeignCredentials, so the two steps compose to "this row exports
// exactly the credential it can legitimately hold".
func restoreExportCredentials(out, acc *store.Account) {
	if out == nil || acc == nil {
		return
	}
	switch strings.ToLower(strings.TrimSpace(acc.AccountType)) {
	case "grok":
		if grokAccountIsOAuth(acc) {
			out.OAuthAccessToken = acc.OAuthAccessToken
			out.OAuthRefreshToken = acc.OAuthRefreshToken
			out.OAuthExpiresAt = acc.OAuthExpiresAt
		}
	case "workbuddy":
		// The access token is short-lived; the refresh token is the durable
		// credential Keycloak rotates.
		out.WorkBuddyAccessToken = acc.WorkBuddyAccessToken
		out.WorkBuddyRefreshToken = acc.WorkBuddyRefreshToken
		out.WorkBuddyExpiresAt = acc.WorkBuddyExpiresAt
	case "qoder":
		// The refresh token is the durable credential, and the runtime pair is
		// derived from it at use time but is what the gateway requires on every
		// request, so both travel with the account.
		out.QoderAccessToken = acc.QoderAccessToken
		out.QoderRefreshToken = acc.QoderRefreshToken
		out.QoderExpiresAt = acc.QoderExpiresAt
		out.QoderRuntimeInfo = acc.QoderRuntimeInfo
		out.QoderRuntimeKey = acc.QoderRuntimeKey
	case "cline":
		// The refresh token is the durable credential; the access token is what
		// the chat endpoint spends.
		out.ClineAccessToken = acc.ClineAccessToken
		out.ClineRefreshToken = acc.ClineRefreshToken
		out.ClineExpiresAt = acc.ClineExpiresAt
	}
}

// redactForeignCredentials clears the credential fields of channels other than
// the account's own.
//
// The account read path gets this for free: accountOutput.MarshalJSON deletes the
// credential keys from the response object, whichever channel they belong to. The
// export marshals the stored record instead of going through that marshaler, so
// it needs the same guarantee expressed as data. It matters because a legacy row
// can hold a value in a slot its own channel never writes — the reason
// RedactQoderOutput clears the generic slots at all — and without this the export
// would publish it.
//
// The generic Token/RefreshToken/ClientCookie/SessionCookie/SessionID/ClientUat
// slots are deliberately left alone. They are not "foreign" for WorkBuddy and
// Qoder: both resolvers fall back to them to parse a credential document written
// before the channel had fields of its own, so clearing them here would drop a
// legacy credential from the export instead of protecting it.
func redactForeignCredentials(acc *store.Account) {
	if acc == nil {
		return
	}
	channel := strings.ToLower(strings.TrimSpace(acc.AccountType))
	// Grok's OAuth pair is restored above for an OAuth account specifically, so
	// an SSO row is treated like any other row that has no claim to it.
	if !(channel == "grok" && grokAccountIsOAuth(acc)) {
		acc.OAuthAccessToken = ""
		acc.OAuthRefreshToken = ""
		acc.OAuthExpiresAt = time.Time{}
	}
	if channel != "workbuddy" {
		acc.WorkBuddyAccessToken = ""
		acc.WorkBuddyRefreshToken = ""
		acc.WorkBuddyExpiresAt = time.Time{}
	}
	if channel != "qoder" {
		acc.QoderAccessToken = ""
		acc.QoderRefreshToken = ""
		acc.QoderExpiresAt = time.Time{}
		acc.QoderRuntimeInfo = ""
		acc.QoderRuntimeKey = ""
	}
	if channel != "cline" {
		acc.ClineAccessToken = ""
		acc.ClineRefreshToken = ""
		acc.ClineExpiresAt = time.Time{}
	}
}

func (a *API) HandleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var exportData ExportData
	if err := json.NewDecoder(r.Body).Decode(&exportData); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	result := ImportResult{Total: len(exportData.Accounts)}

	for _, acc := range exportData.Accounts {
		acc.ID = 0
		acc.RequestCount = 0
		acc.AccountType = strings.ToLower(strings.TrimSpace(acc.AccountType))
		if strings.TrimSpace(acc.AccountType) == "" {
			result.Skipped++
			continue
		}
		if !isSupportedAccountType(acc.AccountType) {
			result.Skipped++
			continue
		}
		if strings.EqualFold(acc.AccountType, "grok") {
			normalizeGrokTokenInput(&acc)
			if !grokAccountIsOAuth(&acc) || !grokAccountHasOAuthCredentials(&acc) {
				slog.Warn("Skipped grok import without Build OAuth credentials", "name", acc.Name)
				result.Skipped++
				continue
			}
		}
		if err := a.store.CreateAccount(r.Context(), &acc); err != nil {
			slog.Warn("Failed to import account", "name", acc.Name, "error", err)
			result.Skipped++
		} else {
			result.Imported++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func generateApiKey() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	b := make([]byte, 48)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		b[i] = charset[n.Int64()]
	}
	return "sk-" + string(b), nil
}

func (a *API) HandleKeys(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		keys, err := a.store.ListApiKeys(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(keys)

	case http.MethodPost:
		var req struct {
			Name          string     `json:"name"`
			AllowedModels []string   `json:"allowed_models"`
			RPMLimit      int        `json:"rpm_limit"`
			MaxConcurrent int        `json:"max_concurrent"`
			ExpiresAt     *time.Time `json:"expires_at"`
			// Billing limit in USD ticks; zero means unlimited.
			BillingLimitUSDTicks int64 `json:"billing_limit_usd_ticks"`
			// BillingPeriodDays rolls the settled usage over; zero means only an
			// explicit reset clears it.
			BillingPeriodDays int `json:"billing_period_days"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		if req.RPMLimit < 0 {
			http.Error(w, "rpm_limit must be greater than or equal to zero", http.StatusBadRequest)
			return
		}
		if req.MaxConcurrent < 0 || req.MaxConcurrent > 1024 {
			http.Error(w, "max_concurrent must be between 0 and 1024", http.StatusBadRequest)
			return
		}
		if req.BillingLimitUSDTicks < 0 || req.BillingLimitUSDTicks > maxApiKeyBillingLimitUSDTicks {
			http.Error(w, "billing_limit_usd_ticks must be between 0 and 9000000000000000", http.StatusBadRequest)
			return
		}
		if req.BillingPeriodDays < 0 || req.BillingPeriodDays > maxApiKeyBillingPeriodDays {
			http.Error(w, "billing_period_days must be between 0 and 3650", http.StatusBadRequest)
			return
		}
		if req.ExpiresAt != nil {
			expiresAt := req.ExpiresAt.UTC()
			if !time.Now().UTC().Before(expiresAt) {
				http.Error(w, "expires_at must be in the future", http.StatusBadRequest)
				return
			}
			req.ExpiresAt = &expiresAt
		}

		fullKey, err := generateApiKey()
		if err != nil {
			slog.Error("Failed to generate api key", "error", err)
			http.Error(w, "failed to generate api key", http.StatusInternalServerError)
			return
		}

		hash := sha256.Sum256([]byte(fullKey))
		hashStr := hex.EncodeToString(hash[:])
		key := store.ApiKey{
			Name:                 req.Name,
			KeyHash:              hashStr,
			KeyFull:              fullKey,
			KeyPrefix:            "sk-",
			KeySuffix:            fullKey[len(fullKey)-4:],
			Enabled:              true,
			AllowedModels:        normalizeAllowedModels(req.AllowedModels),
			RPMLimit:             req.RPMLimit,
			MaxConcurrent:        req.MaxConcurrent,
			ExpiresAt:            req.ExpiresAt,
			BillingLimitUSDTicks: req.BillingLimitUSDTicks,
			BillingPeriodDays:    req.BillingPeriodDays,
		}
		if err := a.store.CreateApiKey(r.Context(), &key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(CreateKeyResponse{
			ID:                   key.ID,
			Key:                  fullKey,
			Name:                 key.Name,
			KeyPrefix:            key.KeyPrefix,
			KeySuffix:            key.KeySuffix,
			Enabled:              key.Enabled,
			AllowedModels:        key.AllowedModels,
			RPMLimit:             key.RPMLimit,
			MaxConcurrent:        key.MaxConcurrent,
			ExpiresAt:            key.ExpiresAt,
			CreatedAt:            key.CreatedAt,
			BillingLimitUSDTicks: key.BillingLimitUSDTicks,
		})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleKeyByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// A trailing action segment is stripped before the id is parsed, so
	// /api/keys/5/reset-usage and /api/keys/5/rotate reach the branches below
	// instead of a 400.
	trimmedPath := strings.TrimSuffix(r.URL.Path, "/")
	action := ""
	for _, candidate := range []string{"reset-usage", "rotate"} {
		if strings.HasSuffix(trimmedPath, "/"+candidate) {
			action = candidate
			trimmedPath = strings.TrimSuffix(trimmedPath, "/"+candidate)
			break
		}
	}
	idStr := strings.TrimPrefix(trimmedPath, "/api/keys/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPost:
		// POST /api/keys/{id}/reset-usage: start a fresh billing period for this
		// key. grok2api resets a key's usage when its period ends; the manual
		// action has to exist too, because a misconfigured limit is otherwise
		// unrecoverable until the period rolls over.
		if action == "reset-usage" {
			resetID := id
			key, err := a.store.GetApiKeyByID(r.Context(), resetID)
			if err != nil {
				if errors.Is(err, store.ErrNoRows) {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := a.store.ResetApiKeyBilling(r.Context(), resetID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			key.BillingUsedUSDTicks = 0
			key.BillingPeriodStartedAt = time.Now().UTC()
			if err := a.store.UpdateApiKey(r.Context(), key); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(key)
			return
		}

		// POST /api/keys/{id}/rotate: mint a new secret for an existing key.
		// Only the hash is stored, so a secret that was not copied at creation
		// can never be shown again -- the list knows only its prefix and
		// suffix. Rotating is the supported way back to a usable secret without
		// losing the key's id, name, policy and settled usage.
		if action == "rotate" {
			key, err := a.store.GetApiKeyByID(r.Context(), id)
			if err != nil {
				if errors.Is(err, store.ErrNoRows) {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			fullKey, err := generateApiKey()
			if err != nil {
				slog.Error("Failed to rotate api key", "error", err)
				http.Error(w, "failed to generate api key", http.StatusInternalServerError)
				return
			}
			hash := sha256.Sum256([]byte(fullKey))
			key.KeyHash = hex.EncodeToString(hash[:])
			key.KeyPrefix = "sk-"
			key.KeySuffix = fullKey[len(fullKey)-4:]
			// UpdateApiKey drops the retired hash index, so the old secret stops
			// authenticating as soon as this succeeds.
			if err := a.store.UpdateApiKey(r.Context(), key); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(CreateKeyResponse{
				ID:                   key.ID,
				Key:                  fullKey,
				Name:                 key.Name,
				KeyPrefix:            key.KeyPrefix,
				KeySuffix:            key.KeySuffix,
				Enabled:              key.Enabled,
				AllowedModels:        key.AllowedModels,
				RPMLimit:             key.RPMLimit,
				MaxConcurrent:        key.MaxConcurrent,
				ExpiresAt:            key.ExpiresAt,
				CreatedAt:            key.CreatedAt,
				BillingLimitUSDTicks: key.BillingLimitUSDTicks,
			})
			return
		}

		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return

	case http.MethodPatch:
		var req UpdateKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Enabled == nil && req.AllowedModels == nil && req.RPMLimit == nil && req.MaxConcurrent == nil &&
			req.BillingLimitUSDTicks == nil && req.BillingPeriodDays == nil && len(req.ExpiresAt) == 0 {
			http.Error(w, "at least one policy field is required", http.StatusBadRequest)
			return
		}
		key, err := a.store.GetApiKeyByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if req.Enabled != nil {
			key.Enabled = *req.Enabled
		}
		if req.AllowedModels != nil {
			key.AllowedModels = normalizeAllowedModels(*req.AllowedModels)
		}
		if req.RPMLimit != nil {
			if *req.RPMLimit < 0 {
				http.Error(w, "rpm_limit must be greater than or equal to zero", http.StatusBadRequest)
				return
			}
			key.RPMLimit = *req.RPMLimit
		}
		if req.MaxConcurrent != nil {
			if *req.MaxConcurrent < 0 || *req.MaxConcurrent > 1024 {
				http.Error(w, "max_concurrent must be between 0 and 1024", http.StatusBadRequest)
				return
			}
			key.MaxConcurrent = *req.MaxConcurrent
		}
		if req.BillingLimitUSDTicks != nil {
			if *req.BillingLimitUSDTicks < 0 || *req.BillingLimitUSDTicks > maxApiKeyBillingLimitUSDTicks {
				http.Error(w, "billing_limit_usd_ticks must be between 0 and 9000000000000000", http.StatusBadRequest)
				return
			}
			key.BillingLimitUSDTicks = *req.BillingLimitUSDTicks
		}
		if req.BillingPeriodDays != nil {
			if *req.BillingPeriodDays < 0 || *req.BillingPeriodDays > maxApiKeyBillingPeriodDays {
				http.Error(w, "billing_period_days must be between 0 and 3650", http.StatusBadRequest)
				return
			}
			key.BillingPeriodDays = *req.BillingPeriodDays
		}
		if len(req.ExpiresAt) > 0 {
			expiresAt, err := parseOptionalExpiry(req.ExpiresAt)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if expiresAt != nil && !time.Now().UTC().Before(*expiresAt) {
				http.Error(w, "expires_at must be in the future", http.StatusBadRequest)
				return
			}
			key.ExpiresAt = expiresAt
		}
		if err := a.store.UpdateApiKey(r.Context(), key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(key)

	case http.MethodDelete:
		if err := a.store.DeleteApiKey(r.Context(), id); err != nil {
			if errors.Is(err, store.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		models, err := a.store.ListModels(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// A bare array is the long-standing contract the bundled admin UI
		// consumes. Asking for a page switches to the paged envelope that
		// grok2api's admin client expects, without breaking the old shape.
		page, pageSize, paged := adminModelPaging(r)
		if !paged {
			json.NewEncoder(w).Encode(models)
			return
		}
		models = filterAdminModels(models, r.URL.Query().Get("search"))
		items, total := paginateAdminModels(models, page, pageSize)
		writeAdminModelEnvelope(w, adminModelListEnvelope{
			Items:    items,
			Page:     page,
			PageSize: pageSize,
			Total:    total,
		})

	case http.MethodPost:
		var m store.Model
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := a.store.CreateModel(r.Context(), &m); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(m)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleModelByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	id := strings.TrimPrefix(r.URL.Path, "/api/models/")
	if id == "" {
		http.Error(w, "Model ID required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		m, err := a.store.GetModel(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNoRows) || err.Error() == "redis: nil" {
				http.Error(w, "Model not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(m)

	case http.MethodPut:
		var patch store.Model
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Admin edits own presentation and enablement fields only. Discovery owns
		// verification, provider, upstream mapping, capabilities and provenance;
		// replacing the whole row here used to make a verified Grok model vanish
		// from the tools picker immediately after renaming it.
		m, err := a.store.GetModel(r.Context(), id)
		if err != nil {
			http.Error(w, "Model not found", http.StatusNotFound)
			return
		}
		m.Channel = patch.Channel
		m.ModelID = patch.ModelID
		m.Name = patch.Name
		m.Status = patch.Status
		m.IsDefault = patch.IsDefault
		m.SortOrder = patch.SortOrder
		if err := a.store.UpdateModel(r.Context(), m); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(m)

	case http.MethodDelete:
		if err := a.store.DeleteModel(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) SetTokenCache(c tokencache.Cache) {
	a.tokenCache = c
}

func (a *API) HandleCacheClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if a.tokenCache == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := a.tokenCache.Clear(r.Context()); err != nil {
		http.Error(w, "Failed to clear cache: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func tokenCacheConfigChanged(before, after *config.Config) bool {
	if before == nil || after == nil {
		return true
	}
	return before.CacheTokenCount != after.CacheTokenCount ||
		before.CacheTTL != after.CacheTTL ||
		before.CacheStrategy != after.CacheStrategy ||
		before.EnableTokenCache != after.EnableTokenCache ||
		before.TokenCacheTTL != after.TokenCacheTTL ||
		before.TokenCacheStrategy != after.TokenCacheStrategy
}

func (a *API) clearTokenCaches(ctx context.Context) {
	if a.tokenCache != nil {
		if err := a.tokenCache.Clear(ctx); err != nil {
			slog.Warn("failed to clear token cache after config update", "error", err)
		}
	}
	if a.promptCache != nil {
		if err := a.promptCache.Clear(ctx); err != nil {
			slog.Warn("failed to clear prompt cache after config update", "error", err)
		}
	}
}

func configPayload(cfg *config.Config) (map[string]interface{}, error) {
	if cfg == nil {
		return map[string]interface{}{}, nil
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	payload := map[string]interface{}{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if v, ok := payload["admin_pass"]; ok {
		payload["admin_password"] = v
	}
	// The static admin token remains a deployment-level compatibility secret,
	// but it is intentionally absent from configuration management. Browser
	// operators use sessions; inference callers use managed API keys.
	delete(payload, "admin_token")
	if rawProxyURL, ok := payload["proxy_url"].(string); !ok || strings.TrimSpace(rawProxyURL) == "" {
		if proxyURL := util.ProxyURLFromConfig(cfg); proxyURL != nil {
			payload["proxy_url"] = proxyURL.String()
		}
	}
	return payload, nil
}

func buildConfigFromPatch(r *http.Request, current *config.Config) (*config.Config, error) {
	base := &config.Config{}
	if current != nil {
		copyCfg := *current
		base = &copyCfg
	}

	baseMap, err := configPayload(base)
	if err != nil {
		return nil, err
	}

	patch := map[string]interface{}{}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		return nil, err
	}

	if _, forbidden := patch["admin_token"]; forbidden {
		return nil, fmt.Errorf("admin_token is deployment-managed and cannot be changed through configuration management")
	}
	if v, ok := patch["admin_password"]; ok {
		patch["admin_pass"] = v
	}

	for key, value := range patch {
		baseMap[key] = normalizeConfigPatchValue(key, value)
	}
	if _, ok := patch["proxy_url"]; ok {
		baseMap["proxy_http"] = ""
		baseMap["proxy_https"] = ""
		baseMap["proxy_user"] = ""
		baseMap["proxy_pass"] = ""
	}

	raw, err := json.Marshal(baseMap)
	if err != nil {
		return nil, err
	}
	var newCfg config.Config
	if err := json.Unmarshal(raw, &newCfg); err != nil {
		return nil, err
	}
	return &newCfg, nil
}

func normalizeConfigPatchValue(key string, value interface{}) interface{} {
	if value == nil {
		return nil
	}

	switch key {
	case "enable_token_refresh", "enable_usage_refresh", "enable_token_count", "cache_token_count",
		"enable_token_cache", "auto_refresh_token", "kiro_use_builtin_proxy",
		"antigravity_use_builtin_proxy",
		"enable_context_compress", "debug_enabled":
		if b, ok := parseBoolish(value); ok {
			return b
		}
	case "retry_delay", "request_timeout", "refresh_interval", "cache_ttl", "token_cache_ttl",
		"redis_db", "token_refresh_interval", "load_balancer_cache_ttl", "concurrency_limit",
		"concurrency_timeout", "max_retries", "credential_retries":
		if i, ok := parseIntish(value); ok {
			return i
		}
	case "proxy_bypass":
		return normalizeProxyBypassValue(value)
	case "proxy_url":
		return strings.TrimSpace(fmt.Sprint(value))
	}

	return value
}

func parseBoolish(value interface{}) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		s := strings.TrimSpace(strings.ToLower(v))
		switch s {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
	case float64:
		return v != 0, true
	}
	return false, false
}

func parseIntish(value interface{}) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil {
			return n, true
		}
	}
	return 0, false
}

func normalizeProxyBypassValue(value interface{}) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s := strings.TrimSpace(fmt.Sprint(item))
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		lines := strings.FieldsFunc(v, func(r rune) bool {
			return r == '\n' || r == ','
		})
		out := make([]string, 0, len(lines))
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" {
				out = append(out, line)
			}
		}
		return out
	default:
		return nil
	}
}

func (a *API) syncAccountAfterCreate(acc store.Account) {
	if !acc.Enabled {
		return
	}

	go func(account store.Account) {
		syncCtx, syncCancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer syncCancel()

		accountStatus, _, syncErr := a.refreshAccountState(syncCtx, &account)
		if syncErr != nil {
			slog.Warn("Initial account sync failed", "account_id", account.ID, "type", account.AccountType, "error", syncErr)
			if accountStatus != "" {
				account.StatusCode = accountStatus
				account.StatusMessage = strings.TrimSpace(syncErr.Error())
				account.LastAttempt = time.Now()
			}
		} else {
			applySuccessfulAccountRefreshStatus(&account, accountStatus)
		}

		if updateErr := a.store.UpdateAccount(context.Background(), &account); updateErr != nil {
			slog.Warn("Failed to persist initial account sync", "account_id", account.ID, "type", account.AccountType, "error", updateErr)
		}
	}(acc)
}

func applySuccessfulAccountRefreshStatus(acc *store.Account, status string) {
	if acc == nil {
		return
	}
	status = strings.TrimSpace(status)
	// The credentials answered the upstream, whatever the verdict: stamp it so a
	// scheduler can tell a verified account from one that was never checked. The
	// policy package owns that pairing so every entrance behaves identically.
	if status == "" {
		// A successful check proves the *credential* works; it says nothing about the
		// allowance. Clearing a spent-allowance verdict here is what made the console
		// show a green account that failed again on the very next request — the check
		// answered for the token, while the verdict that parked the account came from
		// the upstream refusing an actual request.
		if accountHoldsAllowanceVerdict(acc, time.Now()) && allowanceStillSpent(acc) {
			acc.VerifiedAt = time.Now()
			return
		}
		accountpolicy.Success(time.Now()).Apply(acc)
		return
	}
	verdict := accountpolicy.Verdict{Status: status, At: time.Now()}
	if verdict.Scope = accountpolicy.ScopeForStatus(status); verdict.Scope == accountpolicy.ScopeCredential {
		verdict.NeedsLogin = true
	}
	// A verifier that reports only a status has no better explanation than the one
	// already on the record. The reason is the operator's only signal — the account
	// table shows a bare code without it — so an unchanged verdict keeps the
	// specific wording (the verifier returns "402" with nothing else, and a
	// manual check used to wipe the upstream's own explanation).
	//
	// The carry-over is limited to the same status: a new code means the old reason
	// described a different problem and would mislead.
	if strings.TrimSpace(verdict.Message) == "" && strings.TrimSpace(acc.StatusCode) == status {
		verdict.Message = strings.TrimSpace(acc.StatusMessage)
	}
	verdict.Apply(acc)
}

// accountHoldsAllowanceVerdict reports whether the account is parked for an
// allowance the upstream refused, with its reset time still ahead.
//
// The question is "would the selector still hold this account?", so the answer has
// to match the selector: a 402 with a future reset time is out of rotation, and a
// check that cleared the marker would only make the next request re-park it — which
// is what the console showed as an account turning green and then red again.
func accountHoldsAllowanceVerdict(acc *store.Account, now time.Time) bool {
	if acc == nil || strings.TrimSpace(acc.StatusCode) != "402" {
		return false
	}
	if acc.QuotaResetAt.IsZero() {
		return false
	}
	return now.Before(acc.QuotaResetAt)
}

// allowanceStillSpent reports whether the meter still says the allowance is gone.
//
// This is the half that keeps the rule from stranding an account: an operator who
// buys credits is released by the next check rather than waiting for the cycle
// boundary the reset time names. It reads the snapshot the check just refreshed,
// so a top-up is visible immediately.
//
// A failed meter read leaves the previous snapshot in place, and a stale "spent"
// answer holds the account until its reset time. That is the conservative
// direction: the alternative is re-offering an account whose allowance was last
// observed to be gone.
func allowanceStillSpent(acc *store.Account) bool {
	if acc == nil {
		return false
	}
	return acc.UsageLimit > 0 && acc.UsageCurrent <= 0
}

func (a *API) persistConfig(ctx context.Context, current, newCfg *config.Config) error {
	if newCfg == nil {
		return fmt.Errorf("config is nil")
	}
	if a.store == nil {
		return fmt.Errorf("settings store not configured")
	}

	storedCfg := newCfg.Clone()
	config.ApplyHardcoded(storedCfg)
	if _, err := middleware.NewAnonymousAllowlist(storedCfg.AnonymousAllowIPs); err != nil {
		return fmt.Errorf("anonymous_allow_ips: %w", err)
	}

	data, err := json.Marshal(storedCfg)
	if err != nil {
		return err
	}
	if err := a.store.SetSetting(ctx, "config", string(data)); err != nil {
		return err
	}
	cacheChanged := tokenCacheConfigChanged(current, storedCfg)

	// Runtime configs are immutable after publication. Replacing the pointer is
	// atomic; mutating the previously published object would race with request
	// handlers and background jobs reading its fields.
	a.config.Store(storedCfg)
	a.notifyConfigChanged(storedCfg)
	if cacheChanged {
		a.clearTokenCaches(ctx)
	}
	return nil
}
