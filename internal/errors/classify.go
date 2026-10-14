package errors

import (
	"slices"
	"strings"
)

// HasExplicitHTTPStatus checks whether an error string contains an explicit
// reference to the given HTTP status code (e.g. "HTTP 401", "status=429").
func HasExplicitHTTPStatus(lower string, code string) bool {
	code = strings.TrimSpace(code)
	if code == "" || lower == "" {
		return false
	}
	patterns := []string{
		"http " + code,
		"http/1.1 " + code,
		"http/2 " + code,
		"status " + code,
		"status=" + code,
		"status:" + code,
		"statuscode " + code,
		"statuscode=" + code,
		"status code " + code,
		"code " + code,
		"code=" + code,
		"code:" + code,
		"response status " + code,
		"response code " + code,
	}
	return slices.ContainsFunc(patterns, func(p string) bool { return strings.Contains(lower, p) })
}

// statusCodePrefixes lists the account-level codes that callers sometimes encode
// as a bare "<code>" delimiter token on an error message.
var statusCodePrefixes = []string{"401", "402", "403", "404", "429"}

// codeAtDelimiter returns the status code starting at offset start of the
// already-lowercased string, when the code is followed by a delimiter (":"),
// whitespace, or the end of the string. The delimiter requirement keeps longer
// numbers ("4040 widgets") from matching.
func codeAtDelimiter(lower string, start int) (string, bool) {
	if start < 0 || start >= len(lower) {
		return "", false
	}
	for _, code := range statusCodePrefixes {
		if !strings.HasPrefix(lower[start:], code) {
			continue
		}
		rest := lower[start+len(code):]
		if rest == "" || strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, " ") {
			return code, true
		}
	}
	return "", false
}

// scanStatusCode finds a delimiter-bounded status code in the already-lowercased
// string, preferring the leftmost occurrence. Provider layers concatenate the
// upstream status into the error text, so either the leading "<code>: <detail>"
// form or the wrapped "context: <code>: <detail>" form must stay recognised.
func scanStatusCode(lower string) string {
	search := lower
	offset := 0
	for {
		index := strings.Index(search, ":")
		if index < 0 {
			return ""
		}
		position := offset + index + 1
		trimmed := position
		for trimmed < len(lower) && lower[trimmed] == ' ' {
			trimmed++
		}
		if code, ok := codeAtDelimiter(lower, trimmed); ok {
			return code
		}
		offset = position
		search = lower[offset:]
	}
}

// LeadingStatusCode returns the status code encoded in a leading "<code>:<detail>"
// prefix (also "HTTP 401", "status=401" is handled by HasExplicitHTTPStatus), or
// "" when the string carries no such prefix. A bare leading code such as "401"
// counts as well.
func LeadingStatusCode(errStr string) string {
	trimmed := strings.TrimSpace(strings.ToLower(errStr))
	code, ok := codeAtDelimiter(trimmed, 0)
	if !ok {
		return ""
	}
	return code
}

// ClassifyAccountStatus maps an error string to an HTTP status code string
// ("401", "403", "404", "429") or returns "" if the error does not indicate
// a recognisable account-level issue.
func ClassifyAccountStatus(errStr string) string {
	lower := strings.ToLower(errStr)
	// Model name/mapping errors should not poison account status.
	if strings.Contains(lower, "model is not found") || strings.Contains(lower, "model not found") {
		return ""
	}
	// An entitlement refusal means the credential was accepted. It must not be
	// recorded as an account status, or a valid account is disabled by a plan
	// problem — and the alarm says the channel is broken when it is not.
	if strings.Contains(lower, "no usable plan or allowance") || IsQoderEntitlement(lower) || isClineModelEntitlement(lower) {
		return ""
	}
	// A status reason persisted by an admin handler is the wrapped error string,
	// so recognise the code that sits inside the wrap chain as well.
	if code := LeadingStatusCode(lower); code != "" {
		return code
	}
	if code := scanStatusCode(lower); code != "" {
		return code
	}
	switch {
	case HasExplicitHTTPStatus(lower, "401") ||
		strings.Contains(lower, "refresh token is expired") ||
		strings.Contains(lower, "new browser login is required") ||
		strings.Contains(lower, "sign in again") ||
		strings.Contains(lower, "signed out") ||
		strings.Contains(lower, "signed_out") ||
		// The upstream session endpoint answers {"status":"unauthenticated"}
		// without a status code; that body is a refused credential, not a
		// transient failure.
		strings.Contains(lower, "unauthenticated") ||
		strings.Contains(lower, "no active sessions found"):
		return "401"
	case HasExplicitHTTPStatus(lower, "403") || strings.Contains(lower, "forbidden"):
		return "403"
	case HasExplicitHTTPStatus(lower, "404"):
		return "404"
	case HasExplicitHTTPStatus(lower, "402") ||
		IsCreditExhaustion(lower) ||
		strings.Contains(lower, "quota_limit"):
		return "402"
	case
		HasExplicitHTTPStatus(lower, "429") ||
			IsQoderAgentLimit(lower) ||
			strings.Contains(lower, "agentlimitresettime") ||
			strings.Contains(lower, "too many requests") ||
			strings.Contains(lower, "rate limit") ||
			strings.Contains(lower, "rate_limit") ||
			strings.Contains(lower, "no remaining quota") ||
			strings.Contains(lower, "quota exceeded"):
		return "429"
	default:
		return ""
	}
}

// UpstreamErrorClass describes the category and retry semantics of an upstream error.
type UpstreamErrorClass struct {
	Category      string
	Retryable     bool
	SwitchAccount bool
}

// ClassifyUpstreamError categorises an upstream error string into a structured
// class that drives retry and account-switching decisions.
func ClassifyUpstreamError(errStr string) UpstreamErrorClass {
	lower := strings.ToLower(errStr)
	switch {
	case strings.Contains(lower, "context canceled") || strings.Contains(lower, "canceled"):
		return UpstreamErrorClass{Category: "canceled"}
	case strings.Contains(lower, " is not configured") ||
		strings.Contains(lower, "configuration error") ||
		strings.Contains(lower, "client is nil"):
		return UpstreamErrorClass{Category: "configuration"}
	// A safety refusal is the client's content, not a capacity or credential
	// problem: it maps to 400 and must never retry. Labelling it anything
	// retryable lets one rejected prompt walk the whole account pool.
	case strings.Contains(lower, "content policy rejected") ||
		strings.Contains(lower, "datainspectionfailed") ||
		strings.Contains(lower, "input text data may contain"):
		return UpstreamErrorClass{Category: "client"}
	// A 200 stream that delivered nothing is the upstream refusing quietly. It
	// is a protocol-level emptiness, not a rate limit, and replaying it adds
	// load to the condition that produced it.
	case strings.Contains(lower, "empty upstream stream"):
		return UpstreamErrorClass{Category: "protocol"}
	// A request the upstream rejected on its own merits: a replay is identical.
	case strings.Contains(lower, "rejected the request parameters"):
		return UpstreamErrorClass{Category: "client"}
	case strings.Contains(lower, "protocol error") || strings.Contains(lower, "no usable stream events"):
		return UpstreamErrorClass{Category: "protocol"}
	case strings.Contains(lower, "pacing registry capacity reached"):
		return UpstreamErrorClass{Category: "local_overload", Retryable: true}
	// A valid Qoder credential may have exhausted its daily request count even
	// while its credit snapshot still shows a positive balance.
	case strings.Contains(lower, "billing daily count exceeded"):
		return UpstreamErrorClass{Category: "quota_exhausted", Retryable: true, SwitchAccount: true}
	// Business code 112 is a refusal of this account's allowance/plan for the
	// requested model, not a malformed client request. Another account may work.
	case strings.Contains(lower, "no usable plan or allowance"):
		return UpstreamErrorClass{Category: "model_unavailable", Retryable: true, SwitchAccount: true}
	case strings.Contains(lower, "model is not found") ||
		strings.Contains(lower, "model not found") ||
		strings.Contains(lower, "model is not supported") ||
		strings.Contains(lower, "model is not allowed") ||
		strings.Contains(lower, "no_implementation_available") ||
		strings.Contains(lower, "context_window_exceeded") ||
		strings.Contains(lower, "max_token_limit") ||
		strings.Contains(lower, "duplicate request"):
		return UpstreamErrorClass{Category: "client"}
	// A refusal about a resource shared by every account has to be decided before
	// the HTTP-status branches below. Qoder reports business code 10605 inside a
	// 401/403 envelope, so the credential branch used to win: a working account's
	// request was answered as an authentication failure and the handler rotated
	// through the pool, meeting the identical refusal on every account.
	case isUpstreamQueueGate(lower):
		// The upstream admitted the request into a queue and did not serve it.
		// Retryable, never switchable — rotating multiplies one shared refusal —
		// and answered as 429 with the upstream's own retry hint.
		//
		// This used to be folded into upstream_unavailable (503). The upstream is
		// not unavailable: the same hour that produced these refusals also
		// produced hundreds of complete generations, and the refusal names both
		// the queue (isQueued) and the wait (retryAfterSeconds). That is a
		// throttle the caller can act on, not an outage it cannot.
		return UpstreamErrorClass{Category: "upstream_queue", Retryable: true}
	case isUpstreamServiceUnavailable(lower):
		// The service behind the model is down and offered no window at all.
		// Retryable on the account already held, never switchable, and reported
		// to the client as an upstream fault (503).
		return UpstreamErrorClass{Category: "upstream_unavailable", Retryable: true}
	case isSharedUpstreamQueueRefusal(lower):
		// The upstream's own pool says it is throttling. Retryable but
		// deliberately not switchable: rotating multiplies one shared refusal, so
		// the request waits out the upstream's window on the account it holds.
		return UpstreamErrorClass{Category: "rate_limit"}
	case HasExplicitHTTPStatus(lower, "401") ||
		strings.Contains(lower, "refresh token is expired") ||
		strings.Contains(lower, "new browser login is required") ||
		strings.Contains(lower, "sign in again") ||
		strings.Contains(lower, "signed out") ||
		strings.Contains(lower, "signed_out") ||
		strings.Contains(lower, "invalid_api_key"):
		return UpstreamErrorClass{Category: "auth", Retryable: true, SwitchAccount: true}
	case isClineModelEntitlement(lower):
		return UpstreamErrorClass{Category: "model_unavailable", Retryable: true, SwitchAccount: true}
	case HasExplicitHTTPStatus(lower, "403"):
		return UpstreamErrorClass{Category: "auth_blocked", Retryable: true, SwitchAccount: true}
	case HasExplicitHTTPStatus(lower, "404"):
		return UpstreamErrorClass{Category: "auth_blocked"}
	case strings.Contains(lower, "input is too long") || HasExplicitHTTPStatus(lower, "400"):
		return UpstreamErrorClass{Category: "client"}
	case HasExplicitHTTPStatus(lower, "402") ||
		IsCreditExhaustion(lower) ||
		strings.Contains(lower, "quota_limit"):
		return UpstreamErrorClass{Category: "quota_exhausted", Retryable: true, SwitchAccount: true}
	case strings.Contains(lower, "code=6004"):
		return UpstreamErrorClass{Category: "rate_limit", Retryable: true, SwitchAccount: true}
	case HasExplicitHTTPStatus(lower, "429") ||
		IsQoderAgentLimit(lower) ||
		IsQoderModelRateLimited(lower) ||
		// Cline's inference cap arrives as 429 with the wait written in prose
		// ("Try again in 17h 59m"). Recognising the phrase keeps it a rate limit
		// instead of an unknown server fault, and the channel parses the wait.
		IsClineInferenceCap(lower) ||
		strings.Contains(lower, "available upstream accounts are rate-limited") ||
		strings.Contains(lower, "agentlimitresettime") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "rate_limit") ||
		strings.Contains(lower, "no remaining quota") ||
		strings.Contains(lower, "quota exceeded"):
		return UpstreamErrorClass{Category: "rate_limit", Retryable: true, SwitchAccount: true}
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded") || strings.Contains(lower, "context deadline"):
		return UpstreamErrorClass{Category: "timeout", Retryable: true, SwitchAccount: true}
	case strings.Contains(lower, "connection reset") || strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "unexpected eof") || strings.Contains(lower, "use of closed") ||
		strings.Contains(lower, "broken pipe") || strings.HasSuffix(lower, ": eof") || lower == "eof":
		return UpstreamErrorClass{Category: "network", Retryable: true, SwitchAccount: true}
	case HasExplicitHTTPStatus(lower, "500") || HasExplicitHTTPStatus(lower, "502") || HasExplicitHTTPStatus(lower, "503") || HasExplicitHTTPStatus(lower, "504") ||
		strings.Contains(lower, "llm_unavailable") ||
		strings.Contains(lower, "internal_error"):
		return UpstreamErrorClass{Category: "server", Retryable: true, SwitchAccount: true}
	default:
		return UpstreamErrorClass{Category: "unknown", Retryable: true, SwitchAccount: true}
	}
}

// upstreamMarkerText lowercases an error string and removes the JSON escaping
// backslashes a nested payload carries, so a marker is recognised whether the
// body reached us as raw JSON or as a JSON string inside one more envelope:
// both `{"isQueued":true}` and `{\"isQueued\":true}` normalise to the same
// text. Matching the escaped form directly is what made every quoted marker in
// this file dead code — the haystack held `\"isqueued\":true`, which does not
// contain `"isqueued":true`, so only the bare "10605" digits kept the shared
// refusal classified at all.
func upstreamMarkerText(text string) string {
	return strings.ReplaceAll(strings.ToLower(text), `\`, "")
}

// isSharedUpstreamQueueRefusal reports whether the upstream's own pool says it
// is throttling callers. It keys on the phrasing rather than on the flags,
// because this text is the upstream's own prose and carries no queue marker;
// the flagged, windowed form is isUpstreamQueueGate.
func isSharedUpstreamQueueRefusal(text string) bool {
	text = upstreamMarkerText(text)
	return strings.Contains(text, "available upstream accounts are rate-limited") ||
		strings.Contains(text, "available upstream accounts are rate limited")
}

// isUpstreamQueueGate reports whether the upstream put the request in a queue
// and told the caller when to come back.
//
// Qoder's closed gate carries all of: isQueued (the request was queued rather
// than rejected), retryAfterSeconds (the window it wants), and usually
// serviceAvailable:false with queueCount:0. The queue field is what separates a
// throttle from an outage, so it is required here: without it the payload is
// "the service is down", which isUpstreamServiceUnavailable answers as 503.
//
// This distinction was measured, not assumed. Through the gateway, 96 requests
// in one hour returned 200 (0 failures) while the same window still produced
// these refusals; 24h held 364 complete generations against 184 failures. A
// service that is answering four requests in five is throttling this caller,
// and reporting it as unavailable sent operators looking at the upstream while
// the only useful action was to wait out the window.
func isUpstreamQueueGate(text string) bool {
	text = upstreamMarkerText(text)
	if flagValue(text, "isqueued") == "true" {
		return true
	}
	// 10605 is Qoder's queue/busy business code, and "qoder gateway is busy" is
	// the form this gateway renders it as once a parser has unwrapped the
	// envelope. Both name the queue even when the flags were dropped on the way.
	return IsQoderQueueGate(text)
}

// isUpstreamServiceUnavailable reports whether the upstream said the service
// behind the model is down, with no window to wait out.
func isUpstreamServiceUnavailable(text string) bool {
	text = upstreamMarkerText(text)
	if flagValue(text, "isqueued") == "true" {
		// A gate that named its queue is a throttle, not an outage; that case is
		// answered by isUpstreamQueueGate and must not be reported as 503.
		return false
	}
	if flagValue(text, "serviceavailable") == "false" {
		return true
	}
	// Some envelopes carry an empty, refusing queue without naming the service
	// flag. An empty queue with no retry window is the same closed service.
	return flagValue(text, "queuecount") == "0"
}

// flagValue returns the value token that follows name in an already normalised
// payload. Matching the field and its value rather than one spelling of the pair
// keeps every shape working: `"serviceAvailable":false`, `serviceAvailable=false`
// and a re-rendered or nested copy all read the same.
func flagValue(text, name string) string {
	_, after, ok := strings.Cut(text, name)
	if !ok {
		return ""
	}
	rest := strings.TrimLeft(after, `":= `)
	end := strings.IndexAny(rest, `,"} `)
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

func isClineModelEntitlement(lower string) bool {
	if !HasExplicitHTTPStatus(lower, "403") {
		return false
	}
	return strings.Contains(lower, "entitlement") ||
		strings.Contains(lower, "not subscribed to required model plan") ||
		IsClineProductSurface(lower)
}

// IsCreditExhaustion reports whether a message says the account's allowance is
// gone, rather than that one request needs payment.
//
// The distinction decides whether a channel keeps using the account. An exhausted
// allowance is a fact about the account — the upstream refuses every request for
// it, whatever the model — while a per-request payment refusal may just mean the
// caller asked for a paid model on a free plan. Only the first should take the
// account out of rotation, so the phrasing has to be recognised rather than
// inferred from the status code: both arrive as 402, and WorkBuddy's real refusal
// ("Credits exhausted. Please visit the link below to purchase add-on packs")
// reads nothing like a model-scoped one ("insufficient credits for model").
//
// The list lives here so the status classifier and the account policy cannot
// disagree about what "out of credits" means.
func IsCreditExhaustion(errStr string) bool {
	lower := strings.ToLower(errStr)
	for _, phrase := range []string{
		// Qoder names its exhaustion explicitly; see IsQoderQuotaExhausted.
		"no ai credits remaining",
		"insufficient_funds",
		"insufficient funding",
		"available funding is insufficient",
		"out of credits",
		"credits exhausted",
		"run out of credits",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}
