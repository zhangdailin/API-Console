package grok

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

func chatMessageContentText(content interface{}) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []interface{}:
		var b strings.Builder
		for _, part := range v {
			m, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			t := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
			switch t {
			case "text", "input_text":
				if s := strings.TrimSpace(fmt.Sprint(m["text"])); s != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(s)
				}
			}
		}
		return b.String()
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func buildToolsFromOpenAI(tools []chatwire.ToolDef) []map[string]interface{} {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(tools))
	for _, tool := range tools {
		// A hosted tool (web_search / x_search) is forwarded as-is: it is the
		// server-side search the caller asked for, not a function to call back.
		if normalized, native := chatwire.NativeToolTypes[strings.ToLower(strings.TrimSpace(tool.Type))]; native {
			item := map[string]interface{}{"type": normalized}
			for key, value := range tool.Raw {
				switch key {
				case "type", "function":
				default:
					item[key] = value
				}
			}
			out = append(out, item)
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(tool.Type), "function") {
			continue
		}
		name := strings.TrimSpace(fmt.Sprint(tool.Function["name"]))
		if name == "" {
			continue
		}
		item := map[string]interface{}{
			"type":        "function",
			"name":        name,
			"description": strings.TrimSpace(fmt.Sprint(tool.Function["description"])),
			"parameters":  map[string]interface{}{},
		}
		if params, ok := tool.Function["parameters"]; ok && params != nil {
			item["parameters"] = params
		}
		if strict, ok := tool.Function["strict"].(bool); ok {
			item["strict"] = strict
		}
		out = append(out, item)
	}
	return out
}

func buildToolChoiceFromOpenAI(choice interface{}) interface{} {
	switch v := choice.(type) {
	case nil:
		return nil
	case string:
		c := strings.ToLower(strings.TrimSpace(v))
		if c == "" {
			return nil
		}
		return c
	case map[string]interface{}:
		if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(v["type"])), "function") {
			return v
		}
		fn, _ := v["function"].(map[string]interface{})
		name := strings.TrimSpace(fmt.Sprint(fn["name"]))
		if name == "" {
			return v
		}
		return map[string]interface{}{
			"type": "function",
			"name": name,
		}
	default:
		return v
	}
}

func consoleExtractText(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case map[string]interface{}:
		if t := strings.TrimSpace(fmt.Sprint(x["type"])); t == "output_text" || t == "text" || t == "message" {
			if raw := x["text"]; raw != nil {
				if s := strings.TrimSpace(fmt.Sprint(raw)); s != "" && s != "<nil>" {
					return s
				}
			}
			if raw := x["content"]; raw != nil {
				if s := strings.TrimSpace(consoleExtractText(raw)); s != "" {
					return s
				}
			}
			if raw := x["summary"]; raw != nil {
				if s := strings.TrimSpace(consoleExtractText(raw)); s != "" {
					return s
				}
			}
		}
		if t := strings.TrimSpace(fmt.Sprint(x["type"])); t == "message" || t == "response.output_message" {
			if raw := x["content"]; raw != nil {
				if s := strings.TrimSpace(consoleExtractText(raw)); s != "" {
					return s
				}
			}
		}
		for _, key := range []string{"output_text", "content", "output", "text", "message"} {
			if raw := x[key]; raw != nil {
				if s := consoleExtractText(raw); strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
	case []interface{}:
		var b strings.Builder
		for _, item := range x {
			if s := strings.TrimSpace(consoleExtractText(item)); s != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

func consoleExtractMessageText(v interface{}) string {
	switch x := v.(type) {
	case map[string]interface{}:
		if output, ok := x["output"].([]interface{}); ok {
			var text strings.Builder
			for _, item := range output {
				m, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				t := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
				if t != "message" && t != "response.output_message" {
					continue
				}
				for _, raw := range responses.InterfaceSlice(m["content"]) {
					part, _ := raw.(map[string]interface{})
					if kind := chatwire.ParseLooseStringAny(part["type"]); kind == "output_text" || kind == "text" {
						text.WriteString(responses.StreamString(part["text"]))
					}
				}
			}
			return text.String()
		}
	}
	return strings.TrimSpace(consoleExtractText(v))
}

func consoleFlatAnnotations(v interface{}) []map[string]interface{} {
	seen := map[string]struct{}{}
	out := make([]map[string]interface{}, 0)
	add := func(url, title string, start, end int) {
		url = strings.TrimSpace(url)
		if url == "" {
			return
		}
		key := fmt.Sprintf("%s\x00%s\x00%d:%d", url, title, start, end)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, map[string]interface{}{
			"url":         url,
			"title":       strings.TrimSpace(title),
			"start_index": start,
			"end_index":   end,
		})
	}
	var walk func(interface{})
	walk = func(raw interface{}) {
		switch x := raw.(type) {
		case map[string]interface{}:
			t := strings.ToLower(strings.TrimSpace(fmt.Sprint(x["type"])))
			if t == "url_citation" || (x["url"] != nil && (x["title"] != nil || x["start_index"] != nil || x["end_index"] != nil)) {
				add(responses.StreamString(x["url"]), responses.StreamString(x["title"]), responses.InterfaceToInt(x["start_index"]), responses.InterfaceToInt(x["end_index"]))
			}
			if t == "web_search_call" {
				if action, _ := x["action"].(map[string]interface{}); action != nil {
					for _, src := range responses.InterfaceSlice(action["sources"]) {
						if m, _ := src.(map[string]interface{}); m != nil {
							add(responses.StreamString(m["url"]), responses.StreamString(m["title"]), 0, 0)
						}
					}
					if strings.EqualFold(strings.TrimSpace(fmt.Sprint(action["type"])), "open_page") {
						add(responses.StreamString(action["url"]), "", 0, 0)
					}
				}
			}
			for _, key := range []string{"annotation", "annotations", "content", "output", "item", "response", "url_citation"} {
				if child, ok := x[key]; ok {
					walk(child)
				}
			}
		case []interface{}:
			for _, item := range x {
				walk(item)
			}
		}
	}
	walk(v)
	return out
}

func consoleChatAnnotations(flat []map[string]interface{}) []interface{} {
	if len(flat) == 0 {
		return []interface{}{}
	}
	out := make([]interface{}, 0, len(flat))
	for _, ann := range flat {
		out = append(out, map[string]interface{}{
			"type": "url_citation",
			"url_citation": map[string]interface{}{
				"url":         ann["url"],
				"title":       ann["title"],
				"start_index": ann["start_index"],
				"end_index":   ann["end_index"],
			},
		})
	}
	return out
}

func appendUniqueConsoleAnnotations(dst []map[string]interface{}, src []map[string]interface{}) []map[string]interface{} {
	if len(src) == 0 {
		return dst
	}
	seen := make(map[string]struct{}, len(dst)+len(src))
	for _, ann := range dst {
		seen[fmt.Sprintf("%v\x00%v\x00%v:%v", ann["url"], ann["title"], ann["start_index"], ann["end_index"])] = struct{}{}
	}
	for _, ann := range src {
		key := fmt.Sprintf("%v\x00%v\x00%v:%v", ann["url"], ann["title"], ann["start_index"], ann["end_index"])
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		dst = append(dst, ann)
	}
	return dst
}

func consoleUsage(v map[string]interface{}) map[string]interface{} {
	raw, ok := v["usage"].(map[string]interface{})
	if !ok {
		return nil
	}
	prompt := responses.InterfaceToInt(raw["input_tokens"])
	completion := responses.InterfaceToInt(raw["output_tokens"])
	if prompt == 0 {
		prompt = responses.InterfaceToInt(raw["prompt_tokens"])
	}
	if completion == 0 {
		completion = responses.InterfaceToInt(raw["completion_tokens"])
	}
	total := responses.InterfaceToInt(raw["total_tokens"])
	if total == 0 {
		total = prompt + completion
	}
	reasoning := 0
	if details, _ := raw["output_tokens_details"].(map[string]interface{}); details != nil {
		reasoning = responses.InterfaceToInt(details["reasoning_tokens"])
	}
	if reasoning == 0 {
		reasoning = responses.InterfaceToInt(raw["reasoning_tokens"])
	}
	if reasoning == 0 {
		if details, _ := raw["completion_tokens_details"].(map[string]interface{}); details != nil {
			reasoning = responses.InterfaceToInt(details["reasoning_tokens"])
		}
	}
	inputDetails, _ := responses.FirstNonNil(raw["input_tokens_details"], raw["prompt_tokens_details"]).(map[string]interface{})
	cached := min(max(responses.InterfaceToInt(inputDetails["cached_tokens"]), 0), max(prompt, 0))
	normalized := map[string]interface{}{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens": cached,
			"text_tokens":   prompt,
			"audio_tokens":  0,
			"image_tokens":  0,
		},
		"completion_tokens_details": map[string]interface{}{
			"text_tokens":      max(completion-reasoning, 0),
			"audio_tokens":     0,
			"reasoning_tokens": reasoning,
		},
	}
	// Rebuilding the usage object used to drop everything outside the four
	// counts. The upstream also reports what the request cost and how much
	// context it used; the gateway passes those through, and they are the only
	// source for those figures downstream.
	for _, key := range []string{
		"cost_in_usd_ticks", "num_sources_used", "num_server_side_tools_used", "context_details",
	} {
		if value, exists := raw[key]; exists && value != nil {
			normalized[key] = value
		}
	}
	return normalized
}

// finishUpstreamChat completes a chat response after an upstream call: error
// reporting, quota sync, then streaming or collection. Shared by the console and
// CLI chat paths; url is used for both error and request logging and headers is
// evaluated lazily so it is only built on the success path.
//
// It reports a withheld turn: the response was held back by the quality guard,
// nothing reached the client, and the caller may retry it on another account. The
// parked body is returned so the last attempt can still be delivered when the
// retry budget runs out (the default fail-open policy).
//
// qualityAttempt is the 1-based position of this attempt among the request's
// quality attempts; it only labels the journal rows.
func (h *Handler) finishUpstreamChat(ctx context.Context, w http.ResponseWriter, req *chatwire.Request, sess *chatAccountSession, logger *debug.Logger, name, url string, headers func() http.Header, payload map[string]interface{}, resp *http.Response, err error, qualityAttempt int) (*parkedResponse, bool) {
	if err != nil {
		h.auditChatOutcome(ctx, sess.acc, req, chatOutcome{Finish: "error", Err: err})
		slog.Error(name+" chat upstream failed", "url", url, "status", upstreamStatus(err), "error", err)
		if logger != nil {
			logger.LogUpstreamHTTPError(url, upstreamStatus(err), "", err)
		}
		if markAllGrokAccountStatuses(err) {
			h.markAccountStatus(ctx, sess.acc, err)
		}
		writeGrokUpstreamError(w, err)
		return nil, false
	}
	defer resp.Body.Close()
	if recovery := resp.Header.Get("X-Grok2API-Reasoning-Recovery"); recovery != "" {
		w.Header().Set("X-Grok2API-Reasoning-Recovery", recovery)
	}
	if logger != nil {
		if !logger.Capturing() {
			logger.LogUpstreamRequest(url, debugHeaderMap(headers()), payload)
		}
	}
	h.syncGrokQuota(sess.acc, resp.Header)
	// Account defaults are applied to the upstream payload, not client input.
	// Use this attempt's effective effort for quality checks without freezing a
	// default into the original request that later account retries will reuse.
	if req.ReasoningEffort == nil || strings.TrimSpace(*req.ReasoningEffort) == "" {
		reasoning, _ := payload["reasoning"].(map[string]interface{})
		if effort, ok := reasoning["effort"].(string); ok && strings.TrimSpace(effort) != "" {
			attemptReq := *req
			attemptReq.ReasoningEffort = &effort
			req = &attemptReq
		}
	}
	provider := ProviderBuild
	holdEnabled := h.shouldHoldQualityTurn(req, provider)
	qualityStarted := time.Now()
	mode := "collected"
	if req.Stream {
		mode = "stream"
	}
	if holdEnabled {
		h.auditQualityCheck(ctx, sess.acc, req, "checking", mode, qualityAttempt, qualityStarted, chatOutcome{})
	}
	finishQualityCheck := func(result *chatOutcome) {
		result.QualityChecked, result.QualityAttempt = true, qualityAttempt
		status := "passed"
		if result.Err != nil {
			status = "error"
		}
		h.auditQualityCheck(ctx, sess.acc, req, status, mode, qualityAttempt, qualityStarted, *result)
	}

	if req.Stream {
		var hold *buildQualityHold
		if holdEnabled {
			hold = newBuildQualityHold(w, h.qualityHoldPolicy(), nil)
		}
		result := h.streamBuildChatHolding(w, req, resp.Body, hold)
		if hold != nil {
			result.QualityChecked, result.QualityAttempt = true, qualityAttempt
			if !result.Withheld {
				finishQualityCheck(&result)
			}
		}
		if result.Withheld && hold != nil {
			result.WithheldReason = hold.reason
		}
		guard := h.applyBuildQualityGuard(ctx, sess.acc, req, result)
		h.auditChatOutcome(ctx, sess.acc, req, result)
		if result.Withheld && hold != nil {
			h.auditQualityDegraded(ctx, sess.acc, req, result, qualityWithheldTurn{Mode: "stream", Attempt: qualityAttempt, Guard: guard})
			return parkWithOutcome(hold.writer, result), true
		}
		return nil, false
	}

	if !holdEnabled {
		result := h.collectBuildChat(w, req, resp.Body)
		h.applyBuildQualityGuard(ctx, sess.acc, req, result)
		h.auditChatOutcome(ctx, sess.acc, req, result)
		return nil, false
	}
	// A collected response is buffered anyway, so holding it costs nothing: the
	// body is only written once the guard has judged it.
	deferred := newDeferredResponseWriter(w)
	result := h.collectBuildChat(deferred, req, resp.Body)
	result.QualityChecked, result.QualityAttempt = true, qualityAttempt
	if result.Err != nil {
		finishQualityCheck(&result)
		h.applyBuildQualityGuard(ctx, sess.acc, req, result)
		h.auditChatOutcome(ctx, sess.acc, req, result)
		if commitErr := deferred.Commit(); commitErr != nil {
			slog.Debug("held error response commit failed", "error", commitErr)
		}
		return nil, false
	}
	hold := &buildQualityHold{
		writer:  deferred,
		policy:  h.qualityHoldPolicy(),
		started: time.Now(),
		outcome: &result,
	}
	// The hold's verdict comes first: the guard has to know the turn was
	// withheld, and the request row has to say so instead of looking like a
	// delivered answer.
	if verdict, _ := hold.step(true); verdict == qualityWithhold {
		result.Withheld = true
		result.WithheldReason = hold.reason
		result.Quality.Terminal = true
		guard := h.applyBuildQualityGuard(ctx, sess.acc, req, result)
		h.auditChatOutcome(ctx, sess.acc, req, result)
		h.auditQualityDegraded(ctx, sess.acc, req, result, qualityWithheldTurn{Mode: "collected", Attempt: qualityAttempt, Guard: guard})
		return parkWithOutcome(deferred, result), true
	}
	finishQualityCheck(&result)
	h.applyBuildQualityGuard(ctx, sess.acc, req, result)
	h.auditChatOutcome(ctx, sess.acc, req, result)
	if commitErr := deferred.Commit(); commitErr != nil {
		slog.Debug("held response commit failed", "error", commitErr)
	}
	return nil, false
}

func (h *Handler) serveNativeChat(ctx context.Context, w http.ResponseWriter, req *chatwire.Request, spec ModelSpec, sess *chatAccountSession, logger *debug.Logger, build bool) {
	if h == nil || sess == nil || sess.acc == nil || h.buildClient() == nil {
		writeGrokError(w, http.StatusServiceUnavailable, "grok upstream client or account not configured")
		return
	}
	if req.StartedAt.IsZero() {
		req.StartedAt = time.Now()
	}
	payload, err := h.responsesPayloadFromChat(spec, req, true)
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	provider, endpoint, model := ProviderBuild, h.cliBaseURL()+"/responses", spec.UpstreamModel
	ctx = withReasoningDiagnostics(ctx, payload)
	immutable, err := cloneBuildPayload(payload)
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	policy := h.qualityHoldPolicy()
	replayUnsafe := qualityRequestReplayUnsafe(req)
	remaining := h.accountSwitchBudget()
	used := make([]int64, 0, remaining)
	openNext := func(_ []int64) (*chatAccountSession, error) {
		return h.openCLIAccountSession(ctx, used, model)
	}
	request := func() (*http.Response, error) {
		remaining--
		used = append(used, sess.acc.ID)
		// Apply the selected account's model menu on every attempt, including
		// quality retries. Derive it from the original request so one account's
		// defaults or aliases never become the next account's requested effort.
		if prepareErr := prepareBuildPayload(immutable, payload, sess.acc, model); prepareErr != nil {
			return nil, prepareErr
		}
		ctx = withReasoningDiagnostics(ctx, payload)
		attemptStarted := time.Now()
		resp, requestErr := h.buildClient().doResponsesAt(ctx, sess.acc, "/responses", payload)
		if requestErr == nil || !req.ReasoningReplay || !isReasoningReplayDecodeError(requestErr) || preservesClientCompaction(payload, requestErr) {
			return resp, requestErr
		}
		h.auditAttempt(ctx, sess.acc, ProviderBuild, 1, attemptStarted, requestErr, "reasoning_replay_recovery")
		h.clearReasoningReplay(ctx, req.Model, req.PromptCacheKey)
		recoveryAttempt := 1
		lastRecoveryStage := ""
		retryResp, retryErr := recoverChatReasoning(payload, requestErr, func(stage string) (*http.Response, error) {
			recoveryAttempt++
			lastRecoveryStage = stage
			started := time.Now()
			response, failure := h.buildClient().doResponsesAt(ctx, sess.acc, "/responses", payload)
			h.auditAttempt(ctx, sess.acc, ProviderBuild, recoveryAttempt, started, failure, stage)
			return response, failure
		})
		if retryErr == nil && retryResp != nil {
			retryResp.Header.Set("X-Grok2API-Reasoning-Recovery", lastRecoveryStage)
		}
		return retryResp, retryErr
	}
	// Quality hold: a withheld turn never reached the client, so the request can
	// be retried on another account. The budget is separate from (and smaller
	// than) the transport retry budget, because every held attempt costs a full
	// upstream generation. Both kinds of retry consume the same account-attempt
	// budget, so their limits cannot multiply.
	var parked *parkedResponse
	// reasons lists the detector behind each withheld attempt, in order.
	var reasons []string
	for attempt := 0; ; attempt++ {
		limit := remaining
		if replayUnsafe {
			limit = 1
		}
		resp, err := h.retryWithAccountSwitchLimit(ctx, sess, 1500*time.Millisecond, request, openNext, nil, limit)
		body, withheld := h.finishUpstreamChat(ctx, w, req, sess, logger, provider, endpoint, func() http.Header {
			return h.cliHeaders(sess.acc, sess.token)
		}, payload, resp, err, attempt+1)
		if !withheld {
			if attempt > 0 {
				// Earlier turns were withheld and this one was not: the client
				// never saw a degraded answer.
				h.auditQualityResolution(ctx, sess.acc, req, qualityResolutionRetried, "", reasons, policy)
			}
			return
		}
		parked = body
		reason := ""
		if parked != nil {
			reason = parked.outcome.WithheldReason
		}
		reasons = append(reasons, reason)
		// The account that produced the withheld turn; the session moves on below.
		withheldAccount := sess.acc
		action := boundQualityRetry(
			decideQualityRetry(qualityWithhold, attempt, policy.MaxAttempts),
			attempt+1 < policy.MaxAttempts && remaining > 0 && ctx.Err() == nil && !replayUnsafe,
		)
		if action != qualityActionRetry {
			stopped := qualityStopAttemptsExhausted
			switch {
			case replayUnsafe:
				stopped = qualityStopReplayUnsafe
			case ctx.Err() != nil:
				stopped = qualityStopClientGone
			case remaining <= 0:
				stopped = qualityStopAccountBudget
			}
			resolution := h.deliverParkedQualityTurn(w, parked)
			h.auditDeliveredQualityTurn(ctx, withheldAccount, req, parked, resolution)
			h.auditQualityResolution(ctx, withheldAccount, req, resolution, stopped, reasons, policy)
			return
		}
		sess.Close()
		next, switchErr := openNext(used)
		if switchErr != nil {
			// No other account can serve it: deliver the held turn instead of
			// failing a request the upstream completed.
			resolution := h.deliverParkedQualityTurn(w, parked)
			h.auditDeliveredQualityTurn(ctx, withheldAccount, req, parked, resolution)
			h.auditQualityResolution(ctx, withheldAccount, req, resolution, qualityStopNoOtherAccount, reasons, policy)
			return
		}
		sess.acc, sess.token, sess.poolCandidates, sess.release = next.acc, next.token, next.poolCandidates, next.release
		h.auditQualityRetry(ctx, withheldAccount, sess.acc, req, attempt+2)
	}
}

// deliverParkedQualityTurn finishes a request whose attempts were all withheld
// and reports what the client received. It writes the last held response so the
// caller still gets the answer the upstream produced.
func (h *Handler) deliverParkedQualityTurn(w http.ResponseWriter, parked *parkedResponse) string {
	if parked.isEmpty() {
		writeGrokErrorCode(w, http.StatusBadGateway, "quality_degraded", "the upstream response was withheld and could not be delivered")
		return qualityResolutionUndeliverable
	}
	if err := parked.CommitTo(w); err != nil {
		slog.Debug("withheld response delivery failed", "error", err)
	}
	return qualityResolutionDeliveredDegraded
}

// parkWithOutcome parks a held response together with the accounting of the
// turn that produced it.
func parkWithOutcome(writer *deferredResponseWriter, outcome chatOutcome) *parkedResponse {
	parked := writer.Park()
	if parked != nil {
		parked.outcome = outcome
	}
	return parked
}

// auditDeliveredQualityTurn writes the request row for a withheld turn that the
// client received after all.
//
// A withheld attempt is not billed when it is journalled: the client has not
// seen it, and settlement is first-call-wins, so charging it would both bill a
// turn that may never be delivered and discard the cost of the one that is.
// When the retry budget runs out and the policy fails open, this turn becomes
// the answer, and this is where it is billed.
func (h *Handler) auditDeliveredQualityTurn(ctx context.Context, acc *store.Account, req *chatwire.Request, parked *parkedResponse, resolution string) {
	if resolution != qualityResolutionDeliveredDegraded || parked == nil {
		return
	}
	outcome := parked.outcome
	outcome.DeliveredDegraded = true
	h.auditChatOutcome(ctx, acc, req, outcome)
}

const (
	defaultAccountSwitchBudget = 20
	maxAccountSwitchBudget     = 100
)

// retryWithAccountSwitch runs a request in a bounded loop, switching to
// the next account whenever shouldSwitchGrokAccount fires. doRequest issues the
// request against the current session; openNext returns its replacement.
// onSwitch runs after each successful account swap (e.g. to rebuild the request
// payload for the new account).
func (h *Handler) retryWithAccountSwitch(ctx context.Context, sess *chatAccountSession, switchPace time.Duration, doRequest func() (*http.Response, error), openNext func(used []int64) (*chatAccountSession, error), onSwitch func() error) (*http.Response, error) {
	return h.retryWithAccountSwitchLimit(ctx, sess, switchPace, doRequest, openNext, onSwitch, 0)
}

func (h *Handler) retryWithAccountSwitchLimit(ctx context.Context, sess *chatAccountSession, switchPace time.Duration, doRequest func() (*http.Response, error), openNext func(used []int64) (*chatAccountSession, error), onSwitch func() error, hardLimit int) (*http.Response, error) {
	maxAttempts := h.accountSwitchBudget()
	if hardLimit > 0 && maxAttempts > hardLimit {
		maxAttempts = hardLimit
	}

	used := make([]int64, 0)
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		attempt++
		if sess.acc != nil && sess.acc.ID != 0 {
			used = append(used, sess.acc.ID)
		}
		started := time.Now()
		resp, err := doRequest()
		provider := ProviderBuild
		h.auditAttemptDiagnostic(ctx, sess.acc, provider, attempt, started, err, "account_attempt", resp, nil, "")
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		if markAllGrokAccountStatuses(err) {
			h.markAccountStatus(ctx, sess.acc, err)
		}
		if !shouldSwitchGrokAccount(err) || attempt >= maxAttempts {
			return nil, err
		}

		sess.Close()
		pace := switchPace
		var cooldown *syntheticCooldownError
		if errors.As(err, &cooldown) {
			// This attempt never reached upstream. Rotate immediately; sleeping
			// would serialize the same-team skip and hide the cooldown outcome.
			pace = 0
		}
		if !util.SleepWithContext(ctx, pace) {
			return nil, context.Cause(ctx)
		}
		next, switchErr := openNext(used)
		if switchErr != nil {
			return nil, err
		}
		sess.acc = next.acc
		sess.token = next.token
		sess.poolCandidates = next.poolCandidates
		sess.release = next.release
		if onSwitch != nil {
			if err := onSwitch(); err != nil {
				return nil, err
			}
		}
	}
}

func (h *Handler) accountSwitchBudget() int {
	if cfg := h.configSnapshot(); cfg != nil && cfg.AccountSwitchCount > 0 {
		return min(cfg.AccountSwitchCount, maxAccountSwitchBudget)
	}
	return defaultAccountSwitchBudget
}

func (h *Handler) collectBuildChat(w http.ResponseWriter, req *chatwire.Request, body io.Reader) (outcome chatOutcome) {
	var raw map[string]interface{}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		outcome.Err = err
		writeGrokUpstreamError(w, err)
		return
	}
	if raw["error"] != nil || chatwire.ParseLooseStringAny(raw["status"]) == "failed" {
		outcome.Err = responses.Failure(raw)
		writeGrokUpstreamError(w, outcome.Err)
		return
	}
	text := consoleExtractMessageText(raw)
	refusal := responses.ExtractRefusal(raw)
	filter := stopFilter{sequences: req.Stop}
	text = filter.push(text, true)
	reasoning := consoleExtractReasoningText(raw)
	encryptedReasoning := consoleExtractEncryptedReasoning(raw)
	annotations := consoleChatAnnotations(consoleFlatAnnotations(raw))
	// A single pass over the upstream output collects the tool calls, the
	// searches and the reasoning items; each of them used to walk the same slice
	// again. A malformed function_call is recorded and reported after the
	// accounting below, exactly where the old validation loop returned.
	seen := map[string]bool{}
	var toolCalls []map[string]interface{}
	var searches, reasoningItems []interface{}
	var validationErr error
	for _, entry := range responses.InterfaceSlice(raw["output"]) {
		item, _ := entry.(map[string]interface{})
		kind := chatwire.ParseLooseStringAny(item["type"])
		if kind == "function_call" {
			id := util.FirstNonEmpty(chatwire.ParseLooseStringAny(item["call_id"]), chatwire.ParseLooseStringAny(item["id"]))
			name := chatwire.ParseLooseStringAny(item["name"])
			args, validArgs := item["arguments"].(string)
			if id == "" || id == "<nil>" || name == "" || name == "<nil>" || seen[id] || !validArgs || !json.Valid([]byte(args)) {
				validationErr = fmt.Errorf("invalid or duplicate upstream function_call")
			} else {
				seen[id] = true
			}
		}
		if strings.EqualFold(kind, "function_call") {
			if call := consoleToolCallFromItem(item); call != nil {
				toolCalls = append(toolCalls, call)
			}
		}
		if kind == "web_search_call" {
			searches = append(searches, item)
		}
		if kind == "reasoning" {
			reasoningItems = append(reasoningItems, item)
		}
	}
	outcome.Quality = qualitySignals{
		ExpectReasoning: qualityExpectsReasoning(req, false),
		SawReasoning:    strings.TrimSpace(reasoning) != "",
		VisibleChars:    int64(len(text)),
		ReasoningChars:  int64(len(reasoning)),
		EncryptedChars:  int64(len(encryptedReasoning)),
		ToolCalls:       len(toolCalls),
		Terminal:        true,
		// A completed JSON body exposes no upstream text-arrival timing.
		// Parsing latency must not trigger stream-only dump detectors.
		FirstVisibleMS: -1,
	}
	upstreamUsage := consoleUsage(raw)
	if details, _ := upstreamUsage["completion_tokens_details"].(map[string]interface{}); details != nil {
		outcome.Quality.ReasoningTokens = int64(responses.InterfaceToInt(details["reasoning_tokens"]))
	}
	if validationErr != nil {
		outcome.Err = validationErr
		writeGrokUpstreamError(w, outcome.Err)
		return
	}
	message := map[string]interface{}{
		"role":        "assistant",
		"content":     text,
		"refusal":     nil,
		"annotations": annotations,
	}
	if refusal != "" {
		message["refusal"] = refusal
	}
	if len(searches) > 0 {
		message["x_grok_searches"] = searches
	}
	if filter.matched != "" {
		message["stop_sequence"] = filter.matched
	}
	if strings.TrimSpace(reasoning) != "" {
		message["reasoning_content"] = reasoning
	}
	if encryptedReasoning != "" {
		message["reasoning_encrypted_content"] = encryptedReasoning
	}
	if len(reasoningItems) > 1 {
		message["x_grok_reasoning"] = reasoningItems
	}
	finishReason := "stop"
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finishReason = "tool_calls"
		if strings.TrimSpace(text) == "" {
			message["content"] = nil
		}
	}
	if chatwire.ParseLooseStringAny(raw["status"]) == "incomplete" {
		finishReason = "length"
	}
	if filter.matched != "" {
		finishReason = "stop"
	}
	if text == "" && refusal == "" && len(toolCalls) == 0 && finishReason != "length" && filter.matched == "" {
		outcome.Err = fmt.Errorf("upstream completed response with no content or tool calls")
		writeGrokUpstreamError(w, outcome.Err)
		return
	}
	if len(upstreamUsage) > 0 {
		outcome.Usage = upstreamUsage
		outcome.UsageSource = audit.UsageSourceUpstream
	} else {
		outcome.Usage = addReasoningUsage(buildChatUsagePayload(req, text+refusal, toolCalls), reasoning)
		outcome.UsageSource = audit.UsageSourceEstimated
	}
	outcome.Finish = finishReason
	outcome.FirstToken = time.Now()
	resp := map[string]interface{}{
		"id":                 util.FirstNonEmpty(chatwire.ParseLooseStringAny(raw["id"]), "chatcmpl_"+util.RandomHex(8)),
		"object":             "chat.completion",
		"created":            time.Now().Unix(),
		"model":              req.Model,
		"service_tier":       nil,
		"system_fingerprint": "",
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": outcome.Usage,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		outcome.Err = err
		outcome.Finish = "error"
		return
	}
	if req.ReasoningReplay {
		h.captureReasoningReplayFromMap(context.Background(), req.Model, req.PromptCacheKey, raw)
	}
	return
}

// consoleReasoningField concatenates the text of the parts of item[field]. A
// non-empty partType keeps only parts of that type, which is how reasoning
// content differs from the (untyped) summary.
func consoleReasoningField(item map[string]interface{}, field, partType string) string {
	var out strings.Builder
	for _, value := range responses.InterfaceSlice(item[field]) {
		part, _ := value.(map[string]interface{})
		if part == nil {
			continue
		}
		if partType != "" && !strings.EqualFold(strings.TrimSpace(fmt.Sprint(part["type"])), partType) {
			continue
		}
		if text := fmt.Sprint(part["text"]); strings.TrimSpace(text) != "" && text != "<nil>" {
			out.WriteString(text)
		}
	}
	return out.String()
}

func consoleExtractReasoningText(raw map[string]interface{}) string {
	if raw == nil {
		return ""
	}
	var result strings.Builder
	for _, value := range responses.InterfaceSlice(raw["output"]) {
		item, _ := value.(map[string]interface{})
		if item == nil || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(item["type"])), "reasoning") {
			continue
		}
		text := consoleReasoningField(item, "content", "reasoning_text")
		if text == "" {
			text = consoleReasoningField(item, "summary", "")
		}
		result.WriteString(text)
	}
	return result.String()
}

func consoleExtractEncryptedReasoning(raw map[string]interface{}) string {
	if raw == nil {
		return ""
	}
	latest := ""
	for _, value := range responses.InterfaceSlice(raw["output"]) {
		item, _ := value.(map[string]interface{})
		if item == nil || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(item["type"])), "reasoning") {
			continue
		}
		if encrypted := strings.TrimSpace(fmt.Sprint(item["encrypted_content"])); encrypted != "" && encrypted != "<nil>" {
			latest = encrypted
		}
	}
	return latest
}

func consoleToolCallFromItem(raw interface{}) map[string]interface{} {
	item, _ := raw.(map[string]interface{})
	if item == nil || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(item["type"])), "function_call") {
		return nil
	}
	name := strings.TrimSpace(fmt.Sprint(item["name"]))
	if name == "" || name == "<nil>" {
		return nil
	}
	callID := strings.TrimSpace(fmt.Sprint(item["call_id"]))
	if callID == "" || callID == "<nil>" {
		callID = strings.TrimSpace(fmt.Sprint(item["id"]))
	}
	if callID == "" || callID == "<nil>" {
		callID = "call_" + util.RandomHex(12)
	}
	arguments := "{}"
	if rawArgs, ok := item["arguments"]; ok && rawArgs != nil {
		switch v := rawArgs.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				arguments = strings.TrimSpace(v)
			}
		default:
			if buf, err := json.Marshal(v); err == nil {
				arguments = string(buf)
			}
		}
	}
	return map[string]interface{}{
		"id":   callID,
		"type": "function",
		"function": map[string]interface{}{
			"name":      name,
			"arguments": arguments,
		},
	}
}

func consoleUsageFromStreamEvent(ev map[string]interface{}) map[string]interface{} {
	if ev == nil {
		return nil
	}
	if resp, _ := ev["response"].(map[string]interface{}); resp != nil {
		if usage := consoleUsage(resp); len(usage) > 0 {
			return usage
		}
	}
	return consoleUsage(ev)
}

func consoleReasoningDelta(event string, ev map[string]interface{}) string {
	event = strings.ToLower(strings.TrimSpace(event))
	if !strings.Contains(event, "reasoning") || !strings.Contains(event, "delta") {
		return ""
	}
	for _, key := range []string{"delta", "text"} {
		if value, ok := ev[key]; ok && value != nil {
			if text, ok := value.(string); ok && text != "" {
				return text
			}
		}
	}
	return ""
}
