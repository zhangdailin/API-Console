package audit

import (
	"context"
	"log/slog"
	"orchids-api/internal/util"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"encoding/json"

	"github.com/redis/go-redis/v9"
)

// Kind separates the three journals the log centre shows. They share one stream
// (so retention is a single knob) and are filtered by this label.
type Kind string

const (
	// KindRequest records an inference request: what the caller asked for, which
	// channel and account served it, and how it ended.
	KindRequest Kind = "request"
	// KindOperation records an administrative change: who changed what, from
	// where, and how it ended.
	KindOperation Kind = "operation"
	// KindSystem records lifecycle and background decisions that explain why an
	// account changed state (refresh verdicts, probe results).
	KindSystem Kind = "system"
)

// Event represents a single audit log entry.
// UsageSource describes whether token counters came from the upstream provider
// or from the gateway's local estimator. Empty is retained for old/non-usage
// events; request writers should use one of the constants below.
type UsageSource string

const (
	UsageSourceUpstream  UsageSource = "upstream"
	UsageSourceEstimated UsageSource = "estimated"
	UsageSourceNone      UsageSource = "none"
)

type Event struct {
	Timestamp time.Time `json:"timestamp"`
	// Kind is the journal this event belongs to (request/operation/system).
	Kind      Kind   `json:"kind,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Action    string `json:"action"`
	// Actor is the operator or credential that caused a management change. It is
	// empty for inference traffic, which is identified by APIKeyID instead.
	Actor             string      `json:"actor,omitempty"`
	APIKeyID          int64       `json:"api_key_id,omitempty"`
	AccountID         int64       `json:"account_id,omitempty"`
	Model             string      `json:"model,omitempty"`
	Channel           string      `json:"channel,omitempty"`
	Provider          string      `json:"provider,omitempty"`
	Attempt           int         `json:"attempt,omitempty"`
	InputTokens       int         `json:"input_tokens,omitempty"`
	OutputTokens      int         `json:"output_tokens,omitempty"`
	CachedInputTokens int         `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  int         `json:"cache_write_tokens,omitempty"`
	ReasoningTokens   int         `json:"reasoning_tokens,omitempty"`
	TotalTokens       int         `json:"total_tokens,omitempty"`
	UsageSource       UsageSource `json:"usage_source,omitempty"`
	// CostInUSDTicks is the priced cost of this request in USD ticks
	// (1 USD = 10,000,000,000 ticks). PricingModel is the canonical model the
	// rate was resolved to and PricingVersion identifies the rate table that
	// produced it. An empty PricingModel means the row was deliberately not
	// priced, which is distinct from a priced zero.
	CostInUSDTicks int64  `json:"cost_in_usd_ticks,omitempty"`
	PricingModel   string `json:"pricing_model,omitempty"`
	PricingVersion string `json:"pricing_version,omitempty"`
	ClientIP       string `json:"client_ip,omitempty"`
	UserAgent      string `json:"user_agent,omitempty"`
	Duration       int64  `json:"duration_ms,omitempty"`
	// Target names the object a management change touched (account id, key id).
	Target  string `json:"target,omitempty"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	Details string `json:"details,omitempty"`
	// Redacted lists the request fields that were masked before persisting, so a
	// reader knows a change summary is incomplete by design.
	Redacted []string       `json:"redacted,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Logger is the audit logging interface.
type Logger interface {
	Log(ctx context.Context, event Event)
}

// --- Redis Stream Implementation ---

// RedisLogger writes audit events to a Redis Stream with async buffering.
type RedisLogger struct {
	client    *redis.Client
	streamKey string
	maxLen    int64
	eventCh   chan queuedEvent
	mu        sync.Mutex
	closed    bool
	health    Health
	done      chan struct{}
}

// NewRedisLogger creates an audit logger backed by Redis Streams.
func NewRedisLogger(client *redis.Client, prefix string, maxLen int64) *RedisLogger {
	if maxLen <= 0 {
		maxLen = 10000
	}
	l := &RedisLogger{
		client:    client,
		streamKey: prefix + "audit:log",
		maxLen:    maxLen,
		eventCh:   make(chan queuedEvent, 4096),
		done:      make(chan struct{}),
	}
	go l.writeLoop()
	return l
}

// Health describes this process's best-effort sink; it is not a cluster total.
type Health struct {
	Queue       int        `json:"queue"`
	QueuedBytes int        `json:"queued_bytes"`
	Written     uint64     `json:"written"`
	Dropped     uint64     `json:"dropped"`
	WriteFailed uint64     `json:"write_failed"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastFailure *time.Time `json:"last_failure,omitempty"`
	Closed      bool       `json:"closed"`
}
type queuedEvent struct {
	event Event
	data  []byte
}

const maxEventBytes = 32 * 1024
const maxQueuedBytes = 8 * 1024 * 1024

func (l *RedisLogger) Health() Health {
	l.mu.Lock()
	defer l.mu.Unlock()
	health := l.health
	health.Queue = len(l.eventCh)
	health.Closed = l.closed
	return health
}

func (l *RedisLogger) Log(_ context.Context, event Event) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if event.Kind == "" {
		event.Kind = KindRequest
	}
	// Serialize before enqueueing: callers may reuse metadata after Log returns.
	data, err := json.Marshal(event)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || err != nil || len(data) > maxEventBytes || l.health.QueuedBytes+len(data) > maxQueuedBytes {
		l.health.Dropped++
		return
	}
	select {
	case l.eventCh <- queuedEvent{event: event, data: data}:
		l.health.QueuedBytes += len(data)
	default:
		l.health.Dropped++
	}
}

func (l *RedisLogger) Close() {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.eventCh)
	}
	l.mu.Unlock()
	<-l.done
}

func (l *RedisLogger) writeLoop() {
	defer close(l.done)
	for queued := range l.eventCh {
		batch := []queuedEvent{queued}
	collect:
		for len(batch) < 128 {
			select {
			case next, ok := <-l.eventCh:
				if !ok {
					break collect
				}
				batch = append(batch, next)
			default:
				break collect
			}
		}
		l.mu.Lock()
		for _, item := range batch {
			l.health.QueuedBytes -= len(item.data)
		}
		l.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		pipe := l.client.Pipeline()
		commands := make([]*redis.StringCmd, 0, len(batch))
		for _, item := range batch {
			event := item.event
			commands = append(commands, pipe.XAdd(ctx, &redis.XAddArgs{
				Stream: l.streamKey, MaxLen: l.maxLen, Approx: true,
				Values: map[string]any{"data": string(item.data), "action": event.Action, "status": event.Status, "kind": string(event.Kind)},
			}))
		}
		_, err := pipe.Exec(ctx)
		cancel()
		now := time.Now().UTC()
		l.mu.Lock()
		for _, command := range commands {
			if command.Err() != nil {
				l.health.WriteFailed++
				l.health.LastFailure = &now
			} else {
				l.health.Written++
				l.health.LastSuccess = &now
			}
		}
		l.mu.Unlock()
		if err != nil {
			slog.Warn("Audit log write failed")
		}
	}
}

// --- Nop Implementation ---

// NopLogger discards all audit events.
type NopLogger struct{}

func NewNopLogger() *NopLogger                      { return &NopLogger{} }
func (l *NopLogger) Log(_ context.Context, _ Event) {}

// redactedKeys are request-body fields whose value must never reach the log:
// they are live credentials or secrets. The key stays visible (name and type) so
// a reader still sees WHAT was changed, just not the secret itself.
//
// The list is matched by substring rather than exactly, because the config
// endpoints use prefixed names (redis_password, proxy_pass, public_api_key) that
// an exact-match list kept missing.
var redactedKeys = []string{
	"password", "passwd", "pass", "secret", "token", "apikey", "api_key",
	"authorization", "bearer", "credential", "cookie", "private_key", "privatekey",
	"webhook", "signature", "salt", "nonce", "dsn", "connection_string",
	"public_key", "app_key", "admin_token", "session", "sso",
}

const redactedPlaceholder = "<redacted>"

// urlCredentials matches "scheme://user:password@host" inside a URL-valued field.
var urlCredentials = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)([^/@:\s]+):([^/@\s]+)@`)

// sensitiveKey reports whether a key name carries a secret. Matching is
// case-insensitive and substring-based so "redis_password" and "proxy_pass" are
// covered without enumerating every channel's field names.
func sensitiveKey(key string) bool {
	name := strings.ToLower(strings.TrimSpace(key))
	if name == "" {
		return false
	}
	return slices.ContainsFunc(redactedKeys, func(needle string) bool { return strings.Contains(name, needle) })
}

// Redact sanitises a parsed request body for the operation journal. Credential
// fields are replaced, not dropped, so the summary still shows which knob moved.
func Redact(value any) (any, []string) { return redactValue(value, "") }

func redactValue(value any, prefix string) (any, []string) {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		redacted := make([]string, 0, 2)
		for key, item := range typed {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if sensitiveKey(key) {
				out[key] = redactedPlaceholder
				redacted = append(redacted, path)
				continue
			}
			clean, nested := redactValue(item, path)
			out[key] = clean
			redacted = append(redacted, nested...)
		}
		return out, redacted
	case []any:
		out := make([]any, 0, len(typed))
		redacted := make([]string, 0)
		for _, item := range typed {
			clean, nested := redactValue(item, prefix)
			out = append(out, clean)
			redacted = append(redacted, nested...)
		}
		return out, redacted
	case string:
		return redactString(typed, prefix)
	default:
		return value, nil
	}
}

// redactString masks a password embedded in a URL, which a key-name check cannot
// see: "redis://user:secret@host" must not reach the journal intact.
func redactString(value string, path string) (any, []string) {
	if !strings.Contains(value, "://") {
		return value, nil
	}
	matches := urlCredentials.FindStringSubmatch(value)
	if matches == nil {
		return value, nil
	}
	masked := urlCredentials.ReplaceAllString(value, "${1}${2}:"+redactedPlaceholder+"@")
	label := path
	label = util.FirstNonEmptyUntrimmed(label, "url")
	return masked, []string{label + " (embedded password)"}
}

// SummarizeChange renders a redacted, size-bounded change summary for the
// operation journal. It returns the summary and the list of masked fields.
func SummarizeChange(body []byte) (string, []string) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "", nil
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Not JSON (form post, plain text): keep the shape, never the content.
		return "<" + contentTypeOf(trimmed) + " body, " + itoa(len(trimmed)) + " bytes>", nil
	}
	clean, redacted := Redact(parsed)
	encoded, err := json.Marshal(clean)
	if err != nil {
		return "", redacted
	}
	summary := string(encoded)
	const maxSummary = 2048
	if len(summary) > maxSummary {
		summary = summary[:maxSummary] + "…"
	}
	return summary, redacted
}

func contentTypeOf(raw string) string {
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
		return "json"
	}
	if strings.Contains(raw, "=") {
		return "form"
	}
	return "text"
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 10)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
