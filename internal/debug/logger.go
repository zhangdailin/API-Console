package debug

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxDebugStreamLogBytes int64 = 8 << 20
	maxDebugLogDirectories       = 512
)

var debugLoggerCreateCount atomic.Uint64

// Logger records debug logs for one request.
type Logger struct {
	capture    *Capture
	attempt    *UpstreamAttempt
	enabled    bool
	sseEnabled bool
	dir        string
	outFile    *os.File
	outBytes   int64
	mu         sync.Mutex
	startTime  time.Time
}

// New creates a new debug logger.
func New(enabled bool, sseEnabled bool) *Logger {
	if !enabled {
		return &Logger{enabled: false}
	}

	now := time.Now()
	timestamp := now.Format("2006-01-02_15-04-05.000")
	suffix := "0000"
	var randBytes [2]byte
	if _, err := rand.Read(randBytes[:]); err == nil {
		suffix = hex.EncodeToString(randBytes[:])
	}
	dir := filepath.Join("debug-logs", fmt.Sprintf("%s_%s", timestamp, suffix))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return &Logger{enabled: false}
	}
	if count := debugLoggerCreateCount.Add(1); count%64 == 0 {
		pruneDebugLogDirectories("debug-logs", maxDebugLogDirectories)
	}

	return &Logger{
		enabled:    true,
		sseEnabled: sseEnabled,
		dir:        dir,
		startTime:  time.Now(),
	}
}

func pruneDebugLogDirectories(root string, keep int) {
	if keep < 1 {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	if len(dirs) <= keep {
		return
	}
	sort.Strings(dirs)
	for _, name := range dirs[:len(dirs)-keep] {
		_ = os.RemoveAll(filepath.Join(root, name))
	}
}

// LogIncomingRequest records step 1: the incoming Claude API request.
func (l *Logger) LogIncomingRequest(req any) {
	if !l.enabled {
		return
	}
	l.writeJSON("1_claude_request.json", req)
}

// LogEarlyExit records why the request returned early.
func (l *Logger) LogEarlyExit(reason string, details map[string]any) {
	if !l.enabled {
		return
	}
	payload := map[string]any{
		"reason":     reason,
		"elapsed_ms": time.Since(l.startTime).Milliseconds(),
	}
	if details != nil {
		payload["details"] = details
	}
	l.writeJSON("1_early_exit.json", payload)
}

// LogConvertedPrompt records step 2: the converted prompt.
func (l *Logger) LogConvertedPrompt(prompt string) {
	if !l.enabled {
		return
	}
	l.writeFile("2_converted_prompt.md", prompt)
}

// LogUpstreamRequest records step 3: the request sent upstream.
func (l *Logger) LogUpstreamRequest(url string, headers map[string]string, body any) {
	if !l.enabled {
		return
	}

	safeHeaders := make(map[string]string, len(headers))
	for key, value := range headers {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key":
			safeHeaders[key] = "[REDACTED]"
		default:
			safeHeaders[key] = value
		}
	}
	if raw, ok := body.([]byte); ok {
		if json.Valid(raw) {
			body = json.RawMessage(raw)
		} else {
			body = string(raw)
		}
	}
	data := map[string]any{
		"url":     url,
		"headers": safeHeaders,
		"body":    body,
	}
	if l.capture != nil {
		h := http.Header{}
		for k, v := range safeHeaders {
			h.Set(k, v)
		}
		l.UseUpstreamAttempt(beginUpstream(l.capture, "", url, h, body))
		return
	}
	l.writeJSON("3_upstream_request.json", data)
}

// LogUpstreamHTTPError records an upstream HTTP error (the request failed or
// the answer was not 200).
func (l *Logger) LogUpstreamHTTPError(url string, status int, body string, err error) {
	if !l.enabled {
		return
	}
	payload := map[string]any{
		"url":        url,
		"status":     status,
		"body":       body,
		"elapsed_ms": time.Since(l.startTime).Milliseconds(),
	}
	if err != nil {
		payload["error"] = err.Error()
	}
	if l.capture != nil {
		l.mu.Lock()
		a := l.attempt
		l.mu.Unlock()
		if a != nil {
			a.writeJSON("error.json", payload)
			return
		}
		raw, _ := json.Marshal(payload)
		l.capture.Append("3_upstream_http_error.json", string(raw)+"\n")
		return
	}
	l.writeJSON("3_upstream_http_error.json", payload)
}

// LogOutputSSE records step 5: the SSE handed to the client (appended).
func (l *Logger) LogOutputSSE(event string, data string) {
	if !l.enabled || !l.sseEnabled {
		return
	}
	elapsed := time.Since(l.startTime).Milliseconds()
	l.appendStream(&l.outFile, &l.outBytes, "5_client_sse.jsonl", fmt.Sprintf("[%dms] event: %s\ndata: %s\n\n", elapsed, event, data))
}

func (l *Logger) appendStream(file **os.File, written *int64, name, data string) {
	if l.capture != nil {
		l.capture.Append(name, data)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if *file == nil {
		opened, err := os.OpenFile(filepath.Join(l.dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		*file = opened
	}
	l.writeLimitedStream(*file, written, data)
}

func (l *Logger) writeLimitedStream(file *os.File, written *int64, data string) {
	if file == nil || written == nil || *written >= maxDebugStreamLogBytes {
		return
	}
	remaining := maxDebugStreamLogBytes - *written
	if int64(len(data)) > remaining {
		data = data[:remaining]
	}
	n, _ := io.WriteString(file, data)
	*written += int64(n)
}

// LogInputTokenBreakdown records the input token breakdown.
func (l *Logger) LogInputTokenBreakdown(profile string, basePromptTokens, systemContextTokens, historyTokens, toolsTokens, total int) {
	if !l.enabled {
		return
	}

	payload := map[string]any{
		"prompt_profile":        profile,
		"base_prompt_tokens":    basePromptTokens,
		"system_context_tokens": systemContextTokens,
		"history_tokens":        historyTokens,
		"tools_tokens":          toolsTokens,
		"estimated_total":       total,
	}
	l.writeJSON("6_input_token_breakdown.json", payload)
}

// LogSummary records the request summary.
func (l *Logger) LogSummary(inputTokens, outputTokens int, duration time.Duration, stopReason string) {
	if !l.enabled {
		return
	}

	summary := map[string]any{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"total_tokens":  inputTokens + outputTokens,
		"duration_ms":   duration.Milliseconds(),
		"stop_reason":   stopReason,
	}
	l.writeJSON("6_summary.json", summary)
}

// Close closes the log files.
func (l *Logger) Close() {
	if !l.enabled {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.outFile != nil {
		l.outFile.Close()
		l.outFile = nil
	}
}

func (l *Logger) writeJSON(filename string, data any) {
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	l.writeFile(filename, string(jsonData))
}

func (l *Logger) writeFile(filename string, content string) {
	if l.capture != nil {
		l.capture.Set(filename, content)
		return
	}
	os.WriteFile(filepath.Join(l.dir, filename), []byte(content), 0600)
}

func (l *Logger) Capturing() bool { return l != nil && l.capture != nil }
func (l *Logger) UseUpstreamAttempt(a *UpstreamAttempt) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempt = a
}
