package debug

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const DiagnosticRetention = 24 * time.Hour
const maxCaptureBytes = 64 << 10
const maxDiagnosticBundles = 512

type Section struct {
	Name      string `json:"name"`
	Payload   string `json:"payload"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`

	// buf accumulates the section and Payload is filled in Bundle. The original
	// shape was `s.Payload += text`, which copies the whole section on every
	// append: the tee appends once per Write, so a streamed answer arrives one
	// SSE frame at a time and a 64KiB section built from 128-byte frames copied
	// 18MB (measured) to hold 64KiB. A Builder appends in place instead.
	buf   strings.Builder
	spool *os.File
	size  int
}
type Bundle struct {
	RequestID  string    `json:"request_id"`
	Sections   []Section `json:"sections"`
	Bytes      int       `json:"bytes"`
	DurationMS int64     `json:"duration_ms"`
	Truncated  bool      `json:"truncated"`
}
type Capture struct {
	mu        sync.Mutex
	sections  map[string]*Section
	started   time.Time
	requestID string
	duration  *time.Duration
	bytes     int
	truncated bool
	attempts  int
}
type captureKey struct{}

func WithCapture(ctx context.Context, requestID string) (context.Context, *Capture) {
	c := &Capture{sections: map[string]*Section{}, started: time.Now(), requestID: requestID}
	return context.WithValue(ctx, captureKey{}, c), c
}
func FromContext(ctx context.Context) *Capture {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(captureKey{}).(*Capture)
	return c
}
func NewForContext(ctx context.Context, enabled, sse bool) *Logger {
	if c := FromContext(ctx); c != nil {
		return &Logger{enabled: true, sseEnabled: true, capture: c, startTime: time.Now()}
	}
	return New(enabled, sse)
}
func (c *Capture) Append(name, text string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.appendLocked(name, text)
}
func (c *Capture) appendLocked(name, text string) {
	s := c.sections[name]
	if s == nil {
		s = &Section{Name: name}
		c.sections[name] = s
	}
	if s.spool == nil && s.buf.Len()+len(text) > maxCaptureBytes {
		f, err := os.CreateTemp("", "orchids-diagnostic-*")
		if err == nil {
			_, err = f.WriteString(s.buf.String())
			if err == nil {
				s.spool = f
				s.buf.Reset()
			} else {
				f.Close()
				os.Remove(f.Name())
			}
		}
		// Keep the complete in-memory fallback if temporary storage is unavailable.
	}
	var n int
	var err error
	if s.spool != nil {
		n, err = s.spool.WriteString(text)
	} else {
		n, err = s.buf.WriteString(text)
	}
	s.size += n
	c.bytes += n
	if err != nil {
		s.Truncated = true
	}

}
func (c *Capture) Set(name, text string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.sections[name]; old != nil {
		c.bytes -= old.size
		if old.spool != nil {
			old.spool.Close()
			os.Remove(old.spool.Name())
		}
	}
	delete(c.sections, name)
	c.appendLocked(name, text)
}

var credentialPattern = regexp.MustCompile(`(?i)("(?:[a-z0-9_-]*(?:authorization|cookie|api[_-]?key|token|password|secret|session|sso|signature|private[_-]?key)[a-z0-9_-]*)"\s*:\s*)"(?:\\.|[^"\\])*(?:"|$)`)
var bearerPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
var urlPasswordPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^/@:\s]+):([^/@\s]+)@`)
var opaquePattern = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{8,}|eyJ[A-Za-z0-9_.-]{16,})\b`)

// sanitizeCapture masks credentials while preserving prompts and token counts.
// The same sanitizer applies to JSON, SSE fragments and plain text.
//
// Each pass can only match text that already contains its own trigger token, so
// the three expensive ones are gated by a linear prefilter. Ungated they cost
// ~13ms per 64KiB of text (measured: urlPasswordPattern 6.9ms, opaquePattern
// 3.4ms, bearerPattern 3.0ms — a leading `\b` or `(?i)` costs RE2 its
// literal-prefix index); gated they cost ~0.2ms and allocate nothing, a 65x
// saving on every captured section. credentialPattern stays ungated: it runs
// off a cheap literal-prefix index and costs ~0.05ms.
//
// A prefilter must never miss a match. `Bearer` is matched case-insensitively,
// so it is folded before the test; the other two tokens appear literally in
// their patterns. sanitizeCaptureGatedEquivalence_test.go pins this against the
// ungated sweep on a corpus of credential spellings.
func sanitizeCapture(text string) string {
	text = credentialPattern.ReplaceAllString(text, `${1}"[REDACTED]"`)
	if strings.Contains(strings.ToLower(text), "bearer") {
		text = bearerPattern.ReplaceAllString(text, "Bearer [REDACTED]")
	}
	if strings.Contains(text, "://") {
		text = urlPasswordPattern.ReplaceAllString(text, "${1}${2}:[REDACTED]@")
	}
	if strings.Contains(text, "sk-") || strings.Contains(text, "eyJ") {
		text = opaquePattern.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}
func (c *Capture) Bundle() Bundle {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := Bundle{RequestID: c.requestID, DurationMS: time.Since(c.started).Milliseconds(), Sections: []Section{}, Truncated: c.truncated}
	if c.duration != nil {
		b.DurationMS = c.duration.Milliseconds()
	}
	for _, s := range c.sections {
		// Assembled field by field: Section carries a strings.Builder now, and
		// copying a Builder that has already been written to is what it panics on.
		raw := s.buf.String()
		if s.spool != nil {
			data, err := os.ReadFile(s.spool.Name())
			raw = string(data)
			if err != nil {
				s.Truncated = true
			}
		}
		payload := sanitizeCapture(strings.ToValidUTF8(raw, "\uFFFD"))
		sec := Section{Name: s.Name, Payload: payload, Bytes: len(payload), Truncated: s.Truncated}
		b.Bytes += sec.Bytes
		b.Truncated = b.Truncated || sec.Truncated
		b.Sections = append(b.Sections, sec)
	}
	sort.Slice(b.Sections, func(i, j int) bool { return b.Sections[i].Name < b.Sections[j].Name })
	return b
}

// Finish freezes request time before metric/journal persistence.
func (c *Capture) Finish(duration time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.duration == nil {
		c.duration = &duration
	}
}

type DiagnosticStore struct {
	client *redis.Client
	prefix string
}

func NewDiagnosticStore(client *redis.Client, prefix string) *DiagnosticStore {
	return &DiagnosticStore{client: client, prefix: prefix + "diagnostics:"}
}
func (s *DiagnosticStore) key(id string) string {
	hash := sha256.Sum256([]byte(id))
	return s.prefix + hex.EncodeToString(hash[:])
}
func (s *DiagnosticStore) Save(ctx context.Context, b Bundle) error {
	if s == nil || s.client == nil || b.RequestID == "" {
		return nil
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(b.Sections))
	for _, sec := range b.Sections {
		names = append(names, sec.Name)
	}
	index, _ := json.Marshal(map[string]any{"metadata": map[string]any{"sections": names, "bytes": b.Bytes, "truncated": b.Truncated, "retention": "24 小时，最多 512 个请求"}})
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err = zw.Write(raw); err != nil {
		return err
	}
	if err = zw.Close(); err != nil {
		return err
	}
	raw = compressed.Bytes()
	key := s.key(b.RequestID)
	// Retain complete compressed bundles with count and TTL eviction across replicas.
	return s.client.Eval(ctx, `redis.call('SET',KEYS[1],ARGV[1],'EX',ARGV[3]); redis.call('SET',KEYS[2],ARGV[2],'EX',ARGV[3]); redis.call('ZADD',KEYS[3],ARGV[4],KEYS[1]); local n=redis.call('ZCARD',KEYS[3])-tonumber(ARGV[5]); if n>0 then local old=redis.call('ZRANGE',KEYS[3],0,n-1); for _,k in ipairs(old) do redis.call('DEL',k,k..':index'); redis.call('ZREM',KEYS[3],k); end end; redis.call('EXPIRE',KEYS[3],ARGV[3]); return 1`, []string{key, key + ":index", s.prefix + "order"}, string(raw), string(index), int(DiagnosticRetention.Seconds()), time.Now().UnixMilli(), maxDiagnosticBundles).Err()
}
func (s *DiagnosticStore) Get(ctx context.Context, id string) (*Bundle, error) {
	if s == nil || s.client == nil {
		return nil, nil
	}
	raw, err := s.client.Get(ctx, s.key(id)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		zr, e := gzip.NewReader(bytes.NewReader(raw))
		if e != nil {
			return nil, e
		}
		raw, err = io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, err
		}
	}
	var b Bundle
	if err = json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("decode diagnostic bundle: %w", err)
	}
	return &b, nil
}
func (s *DiagnosticStore) Indexes(ctx context.Context, ids []string) (map[string]any, error) {
	out := map[string]any{}
	if s == nil || s.client == nil || len(ids) == 0 {
		return out, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = s.key(id) + ":index"
	}
	values, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	for i, v := range values {
		if raw, ok := v.(string); ok {
			var index any
			if json.Unmarshal([]byte(raw), &index) == nil {
				out[ids[i]] = index
			}
		}
	}
	return out, nil
}

// Close releases private spill files after persistence, including failed saves.
func (c *Capture) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sections {
		if s.spool != nil {
			s.spool.Close()
			os.Remove(s.spool.Name())
			s.spool = nil
		}
	}
}
