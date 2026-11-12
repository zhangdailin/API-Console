package grok

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/config"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/responses"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

func buildToolsFromOpenAI(tools []chatwire.ToolDef) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		// A hosted tool (web_search / x_search) is forwarded as-is: it is the
		// server-side search the caller asked for, not a function to call back.
		if normalized, native := chatwire.NativeToolTypes[strings.ToLower(strings.TrimSpace(tool.Type))]; native {
			item := map[string]any{"type": normalized}
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
		item := map[string]any{
			"type":        "function",
			"name":        name,
			"description": strings.TrimSpace(fmt.Sprint(tool.Function["description"])),
			"parameters":  map[string]any{},
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

func buildToolChoiceFromOpenAI(choice any) any {
	switch v := choice.(type) {
	case nil:
		return nil
	case string:
		c := strings.ToLower(strings.TrimSpace(v))
		if c == "" {
			return nil
		}
		return c
	case map[string]any:
		if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(v["type"])), "function") {
			return v
		}
		fn, _ := v["function"].(map[string]any)
		name := strings.TrimSpace(fmt.Sprint(fn["name"]))
		if name == "" {
			return v
		}
		return map[string]any{
			"type": "function",
			"name": name,
		}
	default:
		return v
	}
}

func responsesExtractText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case map[string]any:
		if t := strings.TrimSpace(fmt.Sprint(x["type"])); t == "output_text" || t == "text" || t == "message" {
			if raw := x["text"]; raw != nil {
				if s := strings.TrimSpace(fmt.Sprint(raw)); s != "" && s != "<nil>" {
					return s
				}
			}
			if raw := x["content"]; raw != nil {
				if s := strings.TrimSpace(responsesExtractText(raw)); s != "" {
					return s
				}
			}
			if raw := x["summary"]; raw != nil {
				if s := strings.TrimSpace(responsesExtractText(raw)); s != "" {
					return s
				}
			}
		}
		if t := strings.TrimSpace(fmt.Sprint(x["type"])); t == "message" || t == "response.output_message" {
			if raw := x["content"]; raw != nil {
				if s := strings.TrimSpace(responsesExtractText(raw)); s != "" {
					return s
				}
			}
		}
		for _, key := range []string{"output_text", "content", "output", "text", "message"} {
			if raw := x[key]; raw != nil {
				if s := responsesExtractText(raw); strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
	case []any:
		var b strings.Builder
		for _, item := range x {
			if s := strings.TrimSpace(responsesExtractText(item)); s != "" {
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

func responsesExtractMessageText(v any) string {
	if x, ok := v.(map[string]any); ok {
		if output, ok := x["output"].([]any); ok {
			var text strings.Builder
			for _, item := range output {
				m, ok := item.(map[string]any)
				if !ok {
					continue
				}
				t := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
				if t != "message" && t != "response.output_message" {
					continue
				}
				for _, raw := range responses.InterfaceSlice(m["content"]) {
					part, _ := raw.(map[string]any)
					if kind := chatwire.ParseLooseStringAny(part["type"]); kind == "output_text" || kind == "text" {
						text.WriteString(responses.StreamString(part["text"]))
					}
				}
			}
			return text.String()
		}
	}
	return strings.TrimSpace(responsesExtractText(v))
}

func responsesFlatAnnotations(v any) []map[string]any {
	seen := map[string]struct{}{}
	out := make([]map[string]any, 0)
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
		out = append(out, map[string]any{
			"url":         url,
			"title":       strings.TrimSpace(title),
			"start_index": start,
			"end_index":   end,
		})
	}
	var walk func(any)
	walk = func(raw any) {
		switch x := raw.(type) {
		case map[string]any:
			t := strings.ToLower(strings.TrimSpace(fmt.Sprint(x["type"])))
			if t == "url_citation" || (x["url"] != nil && (x["title"] != nil || x["start_index"] != nil || x["end_index"] != nil)) {
				add(responses.StreamString(x["url"]), responses.StreamString(x["title"]), responses.InterfaceToInt(x["start_index"]), responses.InterfaceToInt(x["end_index"]))
			}
			if t == "web_search_call" {
				if action, _ := x["action"].(map[string]any); action != nil {
					for _, src := range responses.InterfaceSlice(action["sources"]) {
						if m, _ := src.(map[string]any); m != nil {
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
		case []any:
			for _, item := range x {
				walk(item)
			}
		}
	}
	walk(v)
	return out
}

func responsesChatAnnotations(flat []map[string]any) []any {
	if len(flat) == 0 {
		return []any{}
	}
	out := make([]any, 0, len(flat))
	for _, ann := range flat {
		out = append(out, map[string]any{
			"type": "url_citation",
			"url_citation": map[string]any{
				"url":         ann["url"],
				"title":       ann["title"],
				"start_index": ann["start_index"],
				"end_index":   ann["end_index"],
			},
		})
	}
	return out
}

func appendUniqueResponsesAnnotations(dst []map[string]any, src []map[string]any) []map[string]any {
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

func responsesUsage(v map[string]any) map[string]any {
	raw, ok := v["usage"].(map[string]any)
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
	if details, _ := raw["output_tokens_details"].(map[string]any); details != nil {
		reasoning = responses.InterfaceToInt(details["reasoning_tokens"])
	}
	if reasoning == 0 {
		reasoning = responses.InterfaceToInt(raw["reasoning_tokens"])
	}
	if reasoning == 0 {
		if details, _ := raw["completion_tokens_details"].(map[string]any); details != nil {
			reasoning = responses.InterfaceToInt(details["reasoning_tokens"])
		}
	}
	inputDetails, _ := responses.FirstNonNil(raw["input_tokens_details"], raw["prompt_tokens_details"]).(map[string]any)
	cached := min(max(responses.InterfaceToInt(inputDetails["cached_tokens"]), 0), max(prompt, 0))
	normalized := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": cached,
			"text_tokens":   prompt,
			"audio_tokens":  0,
			"image_tokens":  0,
		},
		"completion_tokens_details": map[string]any{
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
// reporting, quota sync, then streaming or collection. Used by the Build chat
// path; url is used for both error and request logging and headers is
// evaluated lazily so it is only built on the success path.
func (h *Handler) finishUpstreamChat(ctx context.Context, w http.ResponseWriter, req *chatwire.Request, sess *chatAccountSession, logger *debug.Logger, name, url string, headers func() http.Header, payload map[string]any, resp *http.Response, err error) {
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
		return
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

	if req.Stream {
		keepalive := httpserver.NewSSEKeepalive(w, httpserver.SSEKeepaliveInterval, func() { _ = resp.Body.Close() })
		defer keepalive.Close()
		result := h.streamBuildChat(keepalive, req, resp.Body)
		if err := keepalive.Close(); err != nil && result.Err == nil {
			result.Err, result.Finish = err, "error"
		}
		h.auditChatOutcome(ctx, sess.acc, req, result)
		return
	}
	h.auditChatOutcome(ctx, sess.acc, req, h.collectBuildChat(w, req, resp.Body))
}

func (h *Handler) serveNativeChat(ctx context.Context, w http.ResponseWriter, req *chatwire.Request, spec ModelSpec, sess *chatAccountSession, logger *debug.Logger) {
	if h == nil || sess == nil || sess.acc == nil || h.buildClient() == nil {
		writeGrokError(w, http.StatusServiceUnavailable, "grok upstream client or account not configured")
		return
	}
	if req.StartedAt.IsZero() {
		req.StartedAt = time.Now()
	}
	// Authentication replays and reasoning recovery spend this same HTTP budget.
	ctx, _ = upstream.WithAttemptBudget(ctx, config.GenerationMaxAttempts)
	payload, err := h.responsesPayloadFromChat(spec, req, true)
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	provider, endpoint, model := ProviderBuild, h.cliBaseURL()+"/responses", spec.UpstreamModel
	ctx = grokbase.WithReasoningDiagnostics(ctx, payload)
	immutable, err := cloneBuildPayload(payload)
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	replayUnsafe := requestReplayUnsafe(req)
	remaining := h.accountSwitchBudget()
	used := make([]int64, 0, remaining)
	openNext := func(_ []int64) (*chatAccountSession, error) {
		return h.openCLIAccountSession(ctx, used, model)
	}
	request := func() (*http.Response, error) {
		remaining--
		used = append(used, sess.acc.ID)
		// Apply the selected account's model menu on every attempt. Derive it
		// from the original request so one account's defaults or aliases never
		// become the next account's requested effort.
		if prepareErr := prepareBuildPayload(immutable, payload, sess.acc, model); prepareErr != nil {
			return nil, prepareErr
		}
		ctx = grokbase.WithReasoningDiagnostics(ctx, payload)
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
	var allowSwitch func(error) bool
	if replayUnsafe {
		// A hosted tool may already have run: only a refusal before generation
		// moves the request to another account.
		allowSwitch = isFreeUsageExhaustedError
	}
	resp, err := h.retryWithAccountSwitchWhen(ctx, sess, 1500*time.Millisecond, request, openNext, nil, remaining, allowSwitch)
	h.finishUpstreamChat(ctx, w, req, sess, logger, provider, endpoint, func() http.Header {
		return h.cliHeaders(sess.acc, sess.token)
	}, payload, resp, err)
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
	return h.retryWithAccountSwitchWhen(ctx, sess, switchPace, doRequest, openNext, onSwitch, hardLimit, nil)
}

func (h *Handler) retryWithAccountSwitchWhen(ctx context.Context, sess *chatAccountSession, switchPace time.Duration, doRequest func() (*http.Response, error), openNext func(used []int64) (*chatAccountSession, error), onSwitch func() error, hardLimit int, allowSwitch func(error) bool) (*http.Response, error) {
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
		if !shouldSwitchGrokAccount(err) || attempt >= maxAttempts || (allowSwitch != nil && !allowSwitch(err)) {
			return nil, err
		}

		sess.Close()
		pace := switchPace
		if _, ok := errors.AsType[*syntheticCooldownError](err); ok {
			// This attempt never reached upstream. Rotate immediately; sleeping
			// would serialize the same-team skip and hide the cooldown outcome.
			pace = 0
		}
		if !util.RetryWaitFits(ctx, pace) {
			return nil, err
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
	if cfg := h.configSnapshot(); cfg != nil {
		limit := min(config.MaxAccountSwitches, maxAccountSwitchBudget)
		if cfg.AccountSwitchCount > 0 {
			limit = min(limit, cfg.AccountSwitchCount)
		}
		return limit
	}
	return defaultAccountSwitchBudget
}

func (h *Handler) collectBuildChat(w http.ResponseWriter, req *chatwire.Request, body io.Reader) (outcome chatOutcome) {
	var raw map[string]any
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
	text := responsesExtractMessageText(raw)
	refusal := responses.ExtractRefusal(raw)
	filter := stopFilter{sequences: req.Stop}
	text = filter.push(text, true)
	reasoning := responsesExtractReasoningText(raw)
	encryptedReasoning := responsesExtractEncryptedReasoning(raw)
	annotations := responsesChatAnnotations(responsesFlatAnnotations(raw))
	// A single pass over the upstream output collects the tool calls, the
	// searches and the reasoning items; each of them used to walk the same slice
	// again. A malformed function_call is recorded and reported after the
	// accounting below, exactly where the old validation loop returned.
	seen := map[string]bool{}
	var toolCalls []map[string]any
	var searches, reasoningItems []any
	var validationErr error
	for _, entry := range responses.InterfaceSlice(raw["output"]) {
		item, _ := entry.(map[string]any)
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
			if call := responsesToolCallFromItem(item); call != nil {
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
	upstreamUsage := responsesUsage(raw)
	outcome.ServedModel = responsesServedModel(raw)
	if validationErr != nil {
		outcome.Err = validationErr
		writeGrokUpstreamError(w, outcome.Err)
		return
	}
	message := map[string]any{
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
		outcome.Usage = grokbase.AddReasoningUsage(buildChatUsagePayload(req, text+refusal, toolCalls), reasoning)
		outcome.UsageSource = audit.UsageSourceEstimated
	}
	outcome.Finish = finishReason
	outcome.FirstToken = time.Now()
	// The streaming path marks this as text arrives. Without it every
	// non-streaming answer was journaled with visible_output=false.
	outcome.SawText = text != ""
	resp := map[string]any{
		"id":                 util.FirstNonEmpty(chatwire.ParseLooseStringAny(raw["id"]), "chatcmpl_"+util.RandomHex(8)),
		"object":             "chat.completion",
		"created":            time.Now().Unix(),
		"model":              req.Model,
		"service_tier":       nil,
		"system_fingerprint": "",
		"choices": []map[string]any{{
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

// responsesReasoningField concatenates the text of the parts of item[field]. A
// non-empty partType keeps only parts of that type, which is how reasoning
// content differs from the (untyped) summary.
func responsesReasoningField(item map[string]any, field, partType string) string {
	var out strings.Builder
	for _, value := range responses.InterfaceSlice(item[field]) {
		part, _ := value.(map[string]any)
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

func responsesExtractReasoningText(raw map[string]any) string {
	if raw == nil {
		return ""
	}
	var result strings.Builder
	for _, value := range responses.InterfaceSlice(raw["output"]) {
		item, _ := value.(map[string]any)
		if item == nil || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(item["type"])), "reasoning") {
			continue
		}
		text := responsesReasoningField(item, "content", "reasoning_text")
		if text == "" {
			text = responsesReasoningField(item, "summary", "")
		}
		result.WriteString(text)
	}
	return result.String()
}

func responsesExtractEncryptedReasoning(raw map[string]any) string {
	if raw == nil {
		return ""
	}
	latest := ""
	for _, value := range responses.InterfaceSlice(raw["output"]) {
		item, _ := value.(map[string]any)
		if item == nil || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(item["type"])), "reasoning") {
			continue
		}
		if encrypted := strings.TrimSpace(fmt.Sprint(item["encrypted_content"])); encrypted != "" && encrypted != "<nil>" {
			latest = encrypted
		}
	}
	return latest
}

func responsesToolCallFromItem(raw any) map[string]any {
	item, _ := raw.(map[string]any)
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
	return map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}
}

// responsesServedModel returns the model a Responses object, or a stream event
// wrapping one, says it ran. Empty when the upstream did not name one.
func responsesServedModel(v map[string]any) string {
	if response, ok := v["response"].(map[string]any); ok {
		if model := strings.TrimSpace(chatwire.ParseLooseStringAny(response["model"])); model != "" {
			return model
		}
	}
	return strings.TrimSpace(chatwire.ParseLooseStringAny(v["model"]))
}

func responsesUsageFromStreamEvent(ev map[string]any) map[string]any {
	if ev == nil {
		return nil
	}
	if resp, _ := ev["response"].(map[string]any); resp != nil {
		if usage := responsesUsage(resp); len(usage) > 0 {
			return usage
		}
	}
	return responsesUsage(ev)
}

func responsesReasoningDelta(event string, ev map[string]any) string {
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
