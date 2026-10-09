package grok

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"encoding/json"
	"orchids-api/internal/audit"
	"orchids-api/internal/middleware"
	"orchids-api/internal/responses"
	"orchids-api/internal/store"
)

var diagnosticBearer = regexp.MustCompile(`(?i)\bBearer\s+[^\s,;"<>]+`)
var diagnosticCredential = regexp.MustCompile(`(?i)(?:api[_-]?key|access[_-]?token|refresh[_-]?token|authorization|password|secret|cookie|encrypted_content)\s*["']?\s*[:=]\s*(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;}]+)`)
var diagnosticOpaque = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]+|eyJ[A-Za-z0-9_.-]+)\b`)
var diagnosticURL = regexp.MustCompile(`https?://[^\s"<>]+`)

type reasoningDiagnosticsKey struct{}

// Capture only bounded protocol controls from the payload actually sent upstream.
// An omitted effort stays omitted: it does not mean low, medium, or disabled.
func withReasoningDiagnostics(ctx context.Context, payload map[string]interface{}) context.Context {
	values := map[string]string{}
	reasoning, _ := payload["reasoning"].(map[string]interface{})
	if effort, ok := reasoning["effort"].(string); ok {
		switch effort = strings.ToLower(strings.TrimSpace(effort)); effort {
		case "auto", "none", "minimal", "low", "medium", "high", "xhigh", "max":
			values["reasoning_effort"] = effort
		default:
			values["reasoning_effort"] = "other"
		}
	}
	if summary, ok := reasoning["summary"].(string); ok {
		switch summary {
		case "auto", "concise", "detailed":
			values["reasoning_summary"] = summary
		}
	}
	return context.WithValue(ctx, reasoningDiagnosticsKey{}, values)
}

func addReasoningDiagnostics(ctx context.Context, metadata map[string]interface{}) {
	values, _ := ctx.Value(reasoningDiagnosticsKey{}).(map[string]string)
	for key, value := range values {
		metadata[key] = value
	}
}

// Never retain request/output bodies, opaque reasoning, or unrestricted headers.
// Known account secrets are removed before truncation, including escaped forms.
func diagnosticText(text string, acc *store.Account) string {
	if acc != nil {
		// One list, owned by the type that grows credential fields. This scrubber
		// used to carry its own copy covering seven of the fourteen values, so a
		// Qoder runtime key or a WorkBuddy refresh token could be written into the
		// diagnostics it produces — which are persisted in the audit journal.
		for _, secret := range acc.Secrets() {
			if secret == "" {
				continue
			}
			text = strings.ReplaceAll(text, secret, "[redacted]")
			encoded, _ := json.Marshal(secret)
			if len(encoded) > 2 {
				text = strings.ReplaceAll(text, string(encoded[1:len(encoded)-1]), "[redacted]")
			}
		}
	}
	text = diagnosticBearer.ReplaceAllString(text, "Bearer [redacted]")
	text = diagnosticCredential.ReplaceAllString(text, "credential=[redacted]")
	text = diagnosticOpaque.ReplaceAllString(text, "[redacted]")
	text = diagnosticURL.ReplaceAllStringFunc(text, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[url]"
		}
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		return u.String()
	})
	if runes := []rune(text); len(runes) > 512 {
		text = string(runes[:512]) + " [truncated]"
	}
	return text
}

func (h *Handler) auditAttemptDiagnostic(ctx context.Context, acc *store.Account, provider string, attempt int, started time.Time, err error, stage string, resp *http.Response, body []byte, result string) {
	logger := h.auditLoggerSnapshot()
	if logger == nil {
		return
	}
	metadata := map[string]interface{}{"stage": stage, "started_at": started.UTC().Format(time.RFC3339Nano)}
	addReasoningDiagnostics(ctx, metadata)
	status := "ok"
	if result != "" {
		status = result
	}
	if err != nil {
		status = "error"
		metadata["error_kind"] = fmt.Sprint(ClassifyUpstreamError(err))
	}
	var header http.Header
	if resp != nil {
		metadata["http_status"] = resp.StatusCode
		header = resp.Header
		if resp.Request != nil && resp.Request.URL != nil {
			u := *resp.Request.URL
			u.User = nil
			u.RawQuery = ""
			u.Fragment = ""
			metadata["upstream_url"] = diagnosticText(u.String(), acc)
		}
	}
	var upstream *grokUpstreamError
	if errors.As(err, &upstream) {
		metadata["http_status"] = upstream.status
		header = upstream.header
		if len(body) == 0 {
			body = []byte(upstream.body)
		}
	}
	allowed := map[string]string{}
	for _, key := range []string{"Content-Type", "Retry-After", "X-Request-Id", "Request-Id", "CF-Ray", "CF-Mitigated"} {
		if value := header.Get(key); value != "" {
			allowed[key] = diagnosticText(value, acc)
		}
	}
	if len(allowed) > 0 {
		metadata["response_headers"] = allowed
	}
	chain := []interface{}{}
	for current, n := err, 0; current != nil && n < 4; current, n = errors.Unwrap(current), n+1 {
		// Typed HTTP errors are described by the structured body below, not a
		// string containing its raw body and potentially arbitrary extra fields.
		message := current.Error()
		var wrapped *grokUpstreamError
		if errors.As(current, &wrapped) {
			message = "upstream HTTP request rejected"
		}
		chain = append(chain, map[string]interface{}{"type": fmt.Sprintf("%T", current), "message": diagnosticText(message, acc)})
	}
	if len(chain) > 0 {
		metadata["error_chain"] = chain
	}
	var parsed map[string]interface{}
	if len(body) > 0 {
		metadata["response_body_bytes"] = len(body)
		metadata["response_body_sha256"] = fmt.Sprintf("%x", sha256.Sum256(body))
		metadata["response_body_truncated"] = len(body) >= maxUpstreamBodyBytes && upstream != nil
		if json.Unmarshal(body, &parsed) == nil {
			for _, key := range []string{"id", "status"} {
				if value := responses.StreamString(parsed[key]); value != "" {
					metadata["response_"+key] = diagnosticText(value, acc)
				}
			}
			if detail, ok := parsed["error"].(map[string]interface{}); ok {
				safe := map[string]interface{}{}
				for _, key := range []string{"code", "type", "param", "message"} {
					if value := responses.StreamString(detail[key]); value != "" {
						safe[key] = diagnosticText(value, acc)
					}
				}
				metadata["response_error"] = safe
			}
		}
	}
	usageRaw, reported := parsed["usage"].(map[string]interface{})
	metadata["usage_reported"] = reported
	usage := responses.UsageFromChat(usageRaw)
	inputDetails, _ := usage["input_tokens_details"].(map[string]interface{})
	outputDetails, _ := usage["output_tokens_details"].(map[string]interface{})
	accountID := int64(0)
	if acc != nil {
		accountID = acc.ID
	}
	usageSource := audit.UsageSourceNone
	if reported {
		usageSource = audit.UsageSourceUpstream
	}
	logger.Log(ctx, audit.Event{Kind: audit.KindRequest, RequestID: middleware.GetRequestID(ctx), APIKeyID: middleware.APIKeyID(ctx), AccountID: accountID, Action: "grok_upstream_attempt", Channel: "grok", Provider: provider, Attempt: attempt, Duration: time.Since(started).Milliseconds(), Status: status, Metadata: metadata,
		InputTokens: responses.InterfaceToInt(usage["input_tokens"]), OutputTokens: responses.InterfaceToInt(usage["output_tokens"]), CachedInputTokens: responses.InterfaceToInt(inputDetails["cached_tokens"]), ReasoningTokens: responses.InterfaceToInt(outputDetails["reasoning_tokens"]), TotalTokens: responses.InterfaceToInt(usage["total_tokens"]), UsageSource: usageSource})
}
