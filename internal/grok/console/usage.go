package console

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/middleware"
	"orchids-api/internal/pricing"
	"orchids-api/internal/store"
)

// maxTappedBody bounds how much of a JSON (non-stream) answer is kept to read
// its usage; a larger answer is relayed untouched and reports no usage.
const maxTappedBody = 8 << 20

// usageTap watches a relayed answer for the usage the upstream reports: the
// top-level usage of a JSON body, or the last usage carried by an SSE event
// (a chat chunk's usage, or response.completed's response.usage).
type usageTap struct {
	sse      bool
	body     []byte
	overflow bool
	line     []byte
	usage    map[string]any
}

func newUsageTap(contentType string) *usageTap {
	return &usageTap{sse: strings.Contains(strings.ToLower(contentType), "text/event-stream")}
}

func (t *usageTap) Write(p []byte) {
	if !t.sse {
		if len(t.body)+len(p) > maxTappedBody {
			t.overflow = true
			return
		}
		t.body = append(t.body, p...)
		return
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if len(t.line)+len(p) <= maxTappedBody {
				t.line = append(t.line, p...)
			}
			return
		}
		t.line = append(t.line, p[:i]...)
		t.event(t.line)
		t.line = t.line[:0]
		p = p[i+1:]
	}
}

func (t *usageTap) event(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' || !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	var event map[string]any
	if json.Unmarshal(data, &event) != nil {
		return
	}
	if usage, ok := event["usage"].(map[string]any); ok && len(usage) > 0 {
		t.usage = usage
	} else if response, ok := event["response"].(map[string]any); ok {
		if usage, ok := response["usage"].(map[string]any); ok && len(usage) > 0 {
			t.usage = usage
		}
	}
}

// JSON returns the buffered JSON body, or nil for a stream or an overflow.
func (t *usageTap) JSON() map[string]any {
	if t.sse || t.overflow || len(t.body) == 0 {
		return nil
	}
	var payload map[string]any
	if json.Unmarshal(t.body, &payload) != nil {
		return nil
	}
	return payload
}

// Usage is the reported usage, nil when the upstream sent none.
func (t *usageTap) Usage() map[string]any {
	if t.sse {
		if len(t.line) > 0 {
			t.event(t.line)
			t.line = nil
		}
		return t.usage
	}
	if payload := t.JSON(); payload != nil {
		usage, _ := payload["usage"].(map[string]any)
		return usage
	}
	return nil
}

// tokenCounts normalizes Chat Completions and Responses usage objects.
type tokenCounts struct{ input, cached, output, total, reasoning int64 }

func countTokens(usage map[string]any) tokenCounts {
	number := func(m map[string]any, keys ...string) int64 {
		for _, key := range keys {
			switch v := m[key].(type) {
			case float64:
				return int64(v)
			case json.Number:
				n, _ := v.Int64()
				return n
			}
		}
		return 0
	}
	nested := func(keys ...string) map[string]any {
		for _, key := range keys {
			if m, ok := usage[key].(map[string]any); ok {
				return m
			}
		}
		return map[string]any{}
	}
	c := tokenCounts{
		input:     number(usage, "prompt_tokens", "input_tokens"),
		output:    number(usage, "completion_tokens", "output_tokens"),
		total:     number(usage, "total_tokens"),
		cached:    number(nested("prompt_tokens_details", "input_tokens_details"), "cached_tokens"),
		reasoning: number(nested("completion_tokens_details", "output_tokens_details"), "reasoning_tokens"),
	}
	if c.total == 0 {
		c.total = c.input + c.output
	}
	return c
}

// SetAuditLogger installs the audit journal requests are recorded in.
func (h *Handler) SetAuditLogger(logger audit.Logger) {
	if h != nil && logger != nil {
		h.mu.Lock()
		h.auditLogger = logger
		h.mu.Unlock()
	}
}

func (h *Handler) auditLoggerSnapshot() audit.Logger {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.auditLogger
}

// recordText audits one text request and settles its cost against the API
// key's reservation. Only upstream-reported usage is billed.
func (h *Handler) recordText(ctx context.Context, acc *store.Account, row *store.Model, started time.Time, usage map[string]any, failure error) {
	status, message := "success", ""
	if failure != nil {
		status, message = "error", failure.Error()
	}
	source := audit.UsageSourceNone
	if len(usage) > 0 {
		source = audit.UsageSourceUpstream
	}
	counts := countTokens(usage)
	event := audit.Event{
		Kind: audit.KindRequest, RequestID: middleware.GetRequestID(ctx), Action: "grok_request", APIKeyID: middleware.APIKeyID(ctx),
		Model: row.ModelID, Channel: "grok", Provider: ProviderConsole, Status: status, Error: message,
		Duration: time.Since(started).Milliseconds(), UsageSource: source,
		InputTokens: int(counts.input), CachedInputTokens: int(counts.cached), OutputTokens: int(counts.output),
		TotalTokens: int(counts.total), ReasoningTokens: int(counts.reasoning),
	}
	if acc != nil {
		event.AccountID = acc.ID
	}
	if cost, _ := middleware.SettleAPIKeyBilling(ctx, nil, row.UpstreamModel, source, counts.input, counts.cached, counts.output); cost.Model != "" {
		event.CostInUSDTicks, event.PricingModel, event.PricingVersion = cost.CostInUSDTicks, cost.Model, pricing.Version
	}
	if logger := h.auditLoggerSnapshot(); logger != nil {
		logger.Log(ctx, event)
	}
	if failure == nil {
		h.addAccountUsage(ctx, acc, counts.total)
	}
}

// recordMedia books an already priced media result.
func (h *Handler) recordMedia(ctx context.Context, acc *store.Account, row *store.Model, result pricing.Result, priced bool, metadata map[string]any) {
	status := "unpriced"
	if priced && result.CostInUSDTicks > 0 && middleware.SettleAPIKeyBillingResult(ctx, nil, result) {
		status = "success"
	}
	event := audit.Event{
		Kind: audit.KindRequest, RequestID: middleware.GetRequestID(ctx), Action: "grok_media_request", APIKeyID: middleware.APIKeyID(ctx),
		Model: row.ModelID, Channel: "grok", Provider: ProviderConsole, Status: status, UsageSource: audit.UsageSourceUpstream, Metadata: metadata,
	}
	if acc != nil {
		event.AccountID = acc.ID
	}
	if priced {
		event.CostInUSDTicks, event.PricingModel, event.PricingVersion = result.CostInUSDTicks, result.Model, pricing.Version
	}
	if logger := h.auditLoggerSnapshot(); logger != nil {
		logger.Log(ctx, event)
	}
}

// addAccountUsage counts one served request and its tokens on the account.
func (h *Handler) addAccountUsage(ctx context.Context, acc *store.Account, tokens int64) {
	st := h.store()
	if acc == nil || acc.ID == 0 || st == nil {
		return
	}
	// The client may have hung up as the stream ended; the usage is still owed.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := st.IncrementAccountStats(persistCtx, acc.ID, float64(tokens), 1); err != nil {
		slog.Warn("grok console usage persistence failed", "account_id", acc.ID, "error", err)
	}
}
