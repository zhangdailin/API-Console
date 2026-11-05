package grokbase

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
// WithReasoningDiagnostics records the reasoning controls of payload for the
// attempt diagnostics written later under ctx.
func WithReasoningDiagnostics(ctx context.Context, payload map[string]any) context.Context {
	values := map[string]string{}
	reasoning, _ := payload["reasoning"].(map[string]any)
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

// AddReasoningDiagnostics copies the reasoning controls recorded under ctx into
// an audit metadata map.
func AddReasoningDiagnostics(ctx context.Context, metadata map[string]any) {
	values, _ := ctx.Value(reasoningDiagnosticsKey{}).(map[string]string)
	for key, value := range values {
		metadata[key] = value
	}
}

// Never retain request/output bodies, opaque reasoning, or unrestricted headers.
// Known account secrets are removed before truncation, including escaped forms.
// DiagnosticText scrubs credentials, opaque tokens and URL secrets from text
// and bounds its length.
func DiagnosticText(text string, acc *store.Account) string {
	if acc != nil {
		// One list, owned by the type that grows credential fields. This scrubber
		// used to carry its own copy covering only some of the values, so a
		// WorkBuddy refresh token could be written into the
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

// UpstreamFailure is what a plane's typed upstream error reveals: status, a
// sanitized header copy and a bounded body.
type UpstreamFailure struct {
	Status int
	Header http.Header
	Body   string
}

// Attempt describes one upstream attempt for the audit journal.
type Attempt struct {
	Account  *store.Account
	Provider string
	Number   int
	Started  time.Time
	Err      error
	// ErrorKind is the plane's classification of Err.
	ErrorKind string
	Stage     string
	Response  *http.Response
	Body      []byte
	Result    string
	// Upstream extracts the plane's typed upstream error from an error chain.
	Upstream func(error) (UpstreamFailure, bool)
	// IsUpstream reports whether one error in the chain is the plane's typed
	// upstream error, whose message is replaced by the structured fields.
	IsUpstream func(error) bool
	// MaxBodyBytes is the plane's bound on an upstream error body.
	MaxBodyBytes int
}

// AttemptEvent builds the audit event for one upstream attempt. It never
// retains request/output bodies, opaque reasoning, or unrestricted headers.
func AttemptEvent(ctx context.Context, a Attempt) audit.Event {
	acc, err, resp, body, started := a.Account, a.Err, a.Response, a.Body, a.Started
	stage, result, provider, attempt := a.Stage, a.Result, a.Provider, a.Number
	metadata := map[string]any{"stage": stage, "started_at": started.UTC().Format(time.RFC3339Nano)}
	AddReasoningDiagnostics(ctx, metadata)
	status := "ok"
	if result != "" {
		status = result
	}
	if err != nil {
		status = "error"
		metadata["error_kind"] = a.ErrorKind
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
			metadata["upstream_url"] = DiagnosticText(u.String(), acc)
		}
	}
	upstream, isUpstream := UpstreamFailure{}, false
	if a.Upstream != nil {
		upstream, isUpstream = a.Upstream(err)
	}
	if isUpstream {
		metadata["http_status"] = upstream.Status
		header = upstream.Header
		if len(body) == 0 {
			body = []byte(upstream.Body)
		}
	}
	allowed := map[string]string{}
	for _, key := range []string{"Content-Type", "Retry-After", "X-Request-Id", "Request-Id", "CF-Ray", "CF-Mitigated"} {
		if value := header.Get(key); value != "" {
			allowed[key] = DiagnosticText(value, acc)
		}
	}
	if len(allowed) > 0 {
		metadata["response_headers"] = allowed
	}
	chain := []any{}
	for current, n := err, 0; current != nil && n < 4; current, n = errors.Unwrap(current), n+1 {
		// Typed HTTP errors are described by the structured body below, not a
		// string containing its raw body and potentially arbitrary extra fields.
		message := current.Error()
		if a.IsUpstream != nil && a.IsUpstream(current) {
			message = "upstream HTTP request rejected"
		}
		chain = append(chain, map[string]any{"type": fmt.Sprintf("%T", current), "message": DiagnosticText(message, acc)})
	}
	if len(chain) > 0 {
		metadata["error_chain"] = chain
	}
	var parsed map[string]any
	if len(body) > 0 {
		metadata["response_body_bytes"] = len(body)
		metadata["response_body_sha256"] = fmt.Sprintf("%x", sha256.Sum256(body))
		metadata["response_body_truncated"] = len(body) >= a.MaxBodyBytes && isUpstream
		if json.Unmarshal(body, &parsed) == nil {
			for _, key := range []string{"id", "status"} {
				if value := responses.StreamString(parsed[key]); value != "" {
					metadata["response_"+key] = DiagnosticText(value, acc)
				}
			}
			if detail, ok := parsed["error"].(map[string]any); ok {
				safe := map[string]any{}
				for _, key := range []string{"code", "type", "param", "message"} {
					if value := responses.StreamString(detail[key]); value != "" {
						safe[key] = DiagnosticText(value, acc)
					}
				}
				metadata["response_error"] = safe
			}
		}
	}
	usageRaw, reported := parsed["usage"].(map[string]any)
	metadata["usage_reported"] = reported
	usage := responses.UsageFromChat(usageRaw)
	inputDetails, _ := usage["input_tokens_details"].(map[string]any)
	outputDetails, _ := usage["output_tokens_details"].(map[string]any)
	accountID := int64(0)
	if acc != nil {
		accountID = acc.ID
	}
	usageSource := audit.UsageSourceNone
	if reported {
		usageSource = audit.UsageSourceUpstream
	}
	return audit.Event{Kind: audit.KindRequest, RequestID: middleware.GetRequestID(ctx), APIKeyID: middleware.APIKeyID(ctx), AccountID: accountID, Action: "grok_upstream_attempt", Channel: "grok", Provider: provider, Attempt: attempt, Duration: time.Since(started).Milliseconds(), Status: status, Metadata: metadata,
		InputTokens: responses.InterfaceToInt(usage["input_tokens"]), OutputTokens: responses.InterfaceToInt(usage["output_tokens"]), CachedInputTokens: responses.InterfaceToInt(inputDetails["cached_tokens"]), ReasoningTokens: responses.InterfaceToInt(outputDetails["reasoning_tokens"]), TotalTokens: responses.InterfaceToInt(usage["total_tokens"]), UsageSource: usageSource}
}
