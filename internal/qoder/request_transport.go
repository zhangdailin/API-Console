package qoder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"orchids-api/internal/httpclient"
	"strconv"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// applyAuthHeaders sets the signed header set on an inference request.
//
// The header count is conditional: the organization headers are omitted when
// the account has no organization, and the model headers when no model key
// applies. Presence is not cosmetic — the gateway rejects a request that
// carries an empty organization header.
func (c *Client) applyAuthHeaders(req *http.Request, creds Credentials, fields RuntimeFields, requestID, modelKey, modelSource, body, signedPath string) error {
	return c.applyAuthHeadersBytes(req, creds, fields, requestID, modelKey, modelSource, []byte(body), signedPath)
}

func (c *Client) applyAuthHeadersBytes(req *http.Request, creds Credentials, fields RuntimeFields, requestID, modelKey, modelSource string, body []byte, signedPath string) error {
	unixSeconds := strconv.FormatInt(time.Now().Unix(), 10)
	authorization, err := buildCOSYAuthorization(requestID, fields.EncryptUserInfo, c.clientVersion, fields.Key, unixSeconds, body, signedPath)
	if err != nil {
		return err
	}
	// One request-owned backing array replaces a separate allocation per value.
	// Full slice expressions prevent Header.Add from overwriting its neighbour.
	if len(req.Header) == 0 {
		req.Header = make(http.Header, 32)
	}
	var values [32]string
	index := 0
	set := func(key, value string) {
		values[index] = value
		req.Header[textproto.CanonicalMIMEHeaderKey(key)] = values[index : index+1 : index+1]
		index++
	}
	set("Accept", "text/event-stream")
	set("Accept-Language", "*")
	set("Authorization", authorization)
	set("Cache-Control", "no-cache")
	set("Connection", "keep-alive")
	set("Content-Type", "application/json")
	set("Cosy-Business-Product", sceneBusinessProduct)
	set("Cosy-Business-Type", sceneBusinessType)
	set("Cosy-ClientType", sceneClientID)
	set("Cosy-Data-Policy", dataPolicyHeader(c.dataPolicyAgreed()))
	set("Cosy-Date", unixSeconds)
	set("Cosy-Key", fields.Key)
	set("Cosy-MachineId", c.machineID)
	set("Cosy-MachineOS", machineOS)
	set("Cosy-MachineToken", c.machineTokenOr(c.machineID))
	set("Cosy-MachineType", c.machineTypeOr(machineSceneType))
	if orgID := strings.TrimSpace(creds.OrgID); orgID != "" {
		set("Cosy-Organization-Id", orgID)
	}
	if tags := filterTags(creds.OrgTags); len(tags) > 0 {
		set("Cosy-Organization-Tags", strings.Join(tags, ","))
	}
	set("Cosy-Scene", sceneName)
	set("Cosy-User", strings.TrimSpace(creds.UID))
	set("Cosy-Version", c.clientVersion)
	set("Sec-Fetch-Mode", "cors")
	set("Traceparent", util.Traceparent(requestID))
	set("User-Agent", clientUserAgent)
	set("Login-Version", "v2")
	if key := strings.TrimSpace(modelKey); key != "" {
		set("X-Model-Key", key)
		// The source header is gated on the key, not on its own value: the CLI
		// sends it even when the source itself is empty.
		set("X-Model-Source", strings.TrimSpace(modelSource))
	}
	return nil
}

func dataPolicyHeader(agreed bool) string {
	if agreed {
		return "agree"
	}
	return "disagree"
}

func filterTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if trimmed := strings.TrimSpace(tag); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// attemptChat performs one upstream attempt and consumes its stream.
func (c *Client) attemptChat(ctx context.Context, url string, body []byte, model modelEntry, requestID string, fields RuntimeFields, creds Credentials, emit func(upstream.SSEMessage)) (result streamResult, attemptErr error) {
	reqCtx, cancel := util.WithAttemptTimeout(ctx, c.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return streamResult{}, &attemptStreamError{err: fmt.Errorf("build qoder request: %w", err)}
	}
	if err := c.applyAuthHeadersBytes(req, creds, fields, requestID, model.Key, model.Source, body, signPath(url)); err != nil {
		return streamResult{}, &attemptStreamError{err: err}
	}
	attempt := debug.BeginUpstream(ctx, req.Method, req.URL.String(), req.Header, body)
	var traceMetadata map[string]any
	if debug.FromContext(ctx) != nil {
		traceMetadata = map[string]any{"provider": "qoder", "model_key": model.Key, "model_source": model.Source, "host": req.URL.Host, "httpdns_ip": req.Header.Get("X-Qoder-Httpdns-Ip"), "body_bytes": len(body)}
	}
	traceCtx, latency := attempt.Trace(req.Context(), traceMetadata)
	if latency != nil {
		c.stateMu.RLock()
		if c.account != nil {
			latency.Set("account_id", c.account.ID)
		}
		c.stateMu.RUnlock()
		if transport, ok := c.stream.Transport.(*http.Transport); ok {
			proxy := "direct"
			if transport.Proxy != nil {
				if u, e := transport.Proxy(req); e == nil && u != nil {
					proxy = u.Scheme + "://" + u.Host
				}
			}
			latency.Set("proxy", proxy)
		}
		if raw, e := decodeBody(body); e == nil {
			var wire chatBody
			if json.Unmarshal(raw, &wire) == nil {
				latency.Set("parameters", wire.Parameters)
				latency.Set("messages_count", len(wire.Messages))
				latency.Set("tools_count", len(wire.Tools))
				latency.Set("conversation_fingerprint", fmt.Sprintf("%x", sha256.Sum256([]byte(wire.SessionID)))[:12])
			}
		}
	}
	req = req.WithContext(traceCtx)
	defer func() { latency.Finish(attemptErr) }()
	originalEmit := emit
	emit = func(m upstream.SSEMessage) {
		if m.Type == "model.text-delta" {
			latency.Mark("first_text_ms")
		}
		if m.Type == "model.reasoning-delta" {
			latency.Mark("first_reasoning_ms")
		}
		if m.Type == "model.tool-call" || m.Type == "model.tool-call-delta" {
			latency.Mark("first_tool_ms")
		}
		if originalEmit != nil {
			originalEmit(m)
		}
	}
	finishAttempt, err := upstream.BeginAttempt(ctx)
	if err != nil {
		return streamResult{}, err
	}
	defer func() { finishAttempt(attemptErr) }()
	resp, err := c.stream.Do(req)
	attempt.Response(resp, err)
	latency.Response(resp)
	if err != nil {
		// Only a connection-level hiccup is worth another attempt; a bad URL or
		// an untrusted certificate would fail identically every time.
		transient := IsTransientTransport(err)
		return streamResult{}, &attemptStreamError{err: fmt.Errorf("send qoder request: %w", err), retryable: transient, unreachable: transient && ctx.Err() == nil}
	}
	resp.Body = attempt.CaptureBody(resp.Body)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return streamResult{}, classifyStatus(resp.StatusCode, resp.Header.Get("Retry-After"), raw)
	}

	monitored, progress := httpclient.MonitorProgressIdle(resp.Body, c.streamIdle, cancel, "qoder")
	resp.Body = monitored
	result, err = consumeStreamObserved(resp.Body, func(m upstream.SSEMessage) {
		progress()
		if emit != nil {
			emit(m)
		}
	}, func() { latency.Mark("first_sse_ms") }, upstream.ToolDeltasEnabled(ctx))
	for _, key := range []string{"firstTokenDuration", "totalDuration", "serverDuration"} {
		if v, ok := result.Usage[key]; ok {
			latency.Set("upstream_"+key, v)
		}
	}
	if err != nil {
		if _, ok := errors.AsType[*attemptStreamError](err); ok {
			return result, err
		}
		// A busy or unauthorized verdict that arrived as an in-stream frame is
		// still a retry decision.
		switch {
		case errors.Is(err, ErrBusy):
			return result, &attemptStreamError{err: err, retryable: true, busy: true, wait: 2 * time.Second}
		case errors.Is(err, errUpstreamUnauthorized):
			return result, &attemptStreamError{err: err, unauth: true}
		}
		return result, err
	}
	return result, nil
}

// classifyStatus turns an HTTP failure into a retry decision. The gateway
// reports its queue refusal as business code 10605 under a 401, so the code is
// read before the status: refreshing on a busy verdict would burn the account's
// token for nothing.
func classifyStatus(status int, retryAfter string, raw []byte) error {
	code := envelopeCode(raw)
	detail := extractBodyMessage(raw)
	if detail == "" {
		detail = util.Truncate(string(raw), 300)
	}
	wrapped := apiError(http.MethodPost, "chat", status, raw)

	if code == busyCode || sharedQueueRefusal(detail, string(raw)) {
		return &attemptStreamError{err: fmt.Errorf("%w: %v", ErrBusy, wrapped), busy: true, retryable: true, wait: busyWait(retryAfter, raw)}
	}
	// The daily request count is spent, and the gateway reports that under the
	// same 401/403 envelope it uses for a rejected credential. Reading it as an
	// authentication failure rotated the account's OAuth token and replayed the
	// request for a credential that was never the problem.
	if IsDailyCountExceeded(detail, string(raw)) {
		return &attemptStreamError{err: fmt.Errorf("%w: %s", ErrDailyCountExceeded, detail), retryable: true}
	}
	// A 403 that names the pricing page is an entitlement refusal, not a
	// credential failure: retrying and refreshing both change nothing, and
	// classifying it as unauthorized would retire a valid account.
	if DetectNoEntitlement(detail, string(raw)) {
		return &attemptStreamError{err: entitlementError(string(raw))}
	}
	// Business verdicts take precedence over HTTP authentication statuses,
	// just as they do inside an SSE envelope.
	if isDuplicateRequest(detail, string(raw)) {
		return fmt.Errorf("qoder duplicate request")
	}
	if hasAgentLimitReset(detail, string(raw)) {
		return &agentLimitError{resetAt: agentLimitResetAt(detail, string(raw))}
	}
	if IsContentPolicy(string(raw)) {
		return &attemptStreamError{err: contentPolicyError(wrapped.Error())}
	}
	// A 401/403 is a credential verdict even when its body names an
	// authentication_error or permission_error type: reading those markers as a
	// client fault skipped the refresh and failed a recoverable request as a 400.
	if status == http.StatusUnauthorized || status == http.StatusForbidden || IsSessionDead(string(raw)) {
		return &attemptStreamError{err: fmt.Errorf("%w: %v", errUpstreamUnauthorized, wrapped), unauth: true}
	}
	if IsClientFault(string(raw)) {
		return &attemptStreamError{err: fmt.Errorf("%w: %v", ErrClientFault, wrapped)}
	}
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return &attemptStreamError{err: wrapped, retryable: true, wait: retryAfterDelay(retryAfter)}
	}
	// A provider-side hiccup wearing any status is worth one bounded retry on
	// the account that already holds the request.
	if IsTransientUpstreamStatus(status, string(raw)) {
		return &attemptStreamError{err: transientError(wrapped.Error()), retryable: true, wait: retryAfterDelay(retryAfter)}
	}
	if status >= 500 {
		return &attemptStreamError{err: wrapped, retryable: true}
	}
	// The rest are request-side refusals: replaying them changes nothing and
	// only takes a healthy account out of rotation.
	_ = detail
	return &attemptStreamError{err: wrapped}
}

// busyWait preserves the gateway's minimum backoff. The handler's cumulative
// wait budget decides whether it can retry; shortening the hint retries early.
func busyWait(retryAfter string, raw []byte) time.Duration {
	if delay := retryAfterDelay(retryAfter); delay > 0 {
		return delay
	}
	if seconds := nestedInt64(raw, "retryAfterSeconds", 0); seconds > 0 {
		return saturatingWait(seconds, time.Second)
	}
	if millis := nestedInt64(raw, "retryAfterMs", 0); millis > 0 {
		return saturatingWait(millis, time.Millisecond)
	}
	if wait := nestedInt64(raw, "waitTime", 0); wait > 0 {
		// waitTime has historically been milliseconds, while the explicitly named
		// retryAfterSeconds is seconds.
		return saturatingWait(wait, time.Millisecond)
	}
	return 2 * time.Second
}

func nestedInt64(raw []byte, key string, depth int) int64 {
	if depth > 5 || len(bytes.TrimSpace(raw)) == 0 {
		return 0
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if !json.Valid(raw) || decoder.Decode(&value) != nil {
		return 0
	}
	var walk func(any, int) int64
	walk = func(node any, level int) int64 {
		if level > 5 {
			return 0
		}
		switch typed := node.(type) {
		case map[string]any:
			if rawValue, ok := typed[key]; ok {
				switch number := rawValue.(type) {
				case json.Number:
					parsed, _ := number.Int64()
					return parsed
				case string:
					parsed, _ := strconv.ParseInt(strings.TrimSpace(number), 10, 64)
					return parsed
				}
			}
			for _, child := range typed {
				if found := walk(child, level+1); found > 0 {
					return found
				}
			}
		case []any:
			for _, child := range typed {
				if found := walk(child, level+1); found > 0 {
					return found
				}
			}
		case string:
			var nested any
			decoder := json.NewDecoder(strings.NewReader(typed))
			decoder.UseNumber()
			if json.Valid([]byte(typed)) && decoder.Decode(&nested) == nil {
				return walk(nested, level+1)
			}
		}
		return 0
	}
	return walk(value, depth)
}

func retryAfterDelay(value string) time.Duration { return retryAfterDelayAt(value, time.Now()) }

func retryAfterDelayAt(value string, now time.Time) time.Duration {
	return httpclient.ParseRetryAfter(value, now, 0)
}

func saturatingWait(value int64, unit time.Duration) time.Duration {
	if value <= 0 {
		return 0
	}
	const maximum = time.Duration(1<<63 - 1)
	if value > int64(maximum/unit) {
		return maximum
	}
	return time.Duration(value) * unit
}
