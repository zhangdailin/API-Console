package doubao

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/httpclient"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

const (
	defaultBaseURL     = "https://www.doubao.com"
	defaultAccountsURL = "https://accounts.doubao.com"
	defaultUserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

var chromeVersion = regexp.MustCompile(`Chrome/(\d+)`)

type Client struct {
	acc  *store.Account
	cfg  *config.Config
	http *http.Client
}

func NewFromAccount(acc *store.Account, cfg *config.Config) *Client {
	var copied *store.Account
	if acc != nil {
		copied = acc.Clone()
	}
	proxy := httpclient.ProxyFuncFromConfig(cfg)
	key := "doubao|" + httpclient.GenerateProxyKeyFromConfig(cfg)
	return &Client{acc: copied, cfg: cfg, http: httpclient.GetSharedBrowserHTTPClientWithLimits(key, 0, 0, proxy, cfg)}
}

func (c *Client) Close() {}

func (c *Client) baseURL() string {
	if c != nil && c.cfg != nil && strings.TrimSpace(c.cfg.DoubaoBaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.cfg.DoubaoBaseURL), "/")
	}
	return defaultBaseURL
}

func (c *Client) accountsURL() string {
	if c != nil && c.cfg != nil && strings.TrimSpace(c.cfg.DoubaoAccountsURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.cfg.DoubaoAccountsURL), "/")
	}
	return defaultAccountsURL
}

func (c *Client) userAgent() string {
	if c != nil && c.cfg != nil && strings.TrimSpace(c.cfg.DoubaoUserAgent) != "" {
		return strings.TrimSpace(c.cfg.DoubaoUserAgent)
	}
	return defaultUserAgent
}

func (c *Client) jar() cookieJar {
	if c == nil || c.acc == nil {
		return cookieJar{}
	}
	return decodeCookies(c.acc.ClientCookie)
}

func (c *Client) headers(jar cookieJar, passport bool) http.Header {
	h := http.Header{}
	h.Set("User-Agent", c.userAgent())
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9")
	h.Set("Accept-Encoding", "gzip, deflate, br")
	h.Set("Referer", c.baseURL()+"/chat/")
	h.Set("Origin", c.baseURL())
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	major := "131"
	if match := chromeVersion.FindStringSubmatch(c.userAgent()); len(match) == 2 {
		major = match[1]
	}
	h.Set("Sec-Ch-Ua", `"Google Chrome";v="`+major+`", "Chromium";v="`+major+`", "Not_A Brand";v="24"`)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	h.Set("X-Flow-Trace", "04-"+randomHex(12)+"-"+randomHex(8)+"-01")
	h.Set("Agw-Js-Conv", "str, str")
	if cookie := jar.header(!passport); cookie != "" {
		h.Set("Cookie", cookie)
	}
	if passport {
		// Passport is hosted at accounts.doubao.com while the page itself is on
		// www.doubao.com. They share a site, but not an origin; claiming
		// same-origin is a browser-fingerprint mismatch and can be rejected.
		h.Set("Referer", c.baseURL()+"/")
		h.Set("Sec-Fetch-Site", "same-site")
		h.Set("Accept", "application/json, text/plain, */*")
		h.Set("Content-Type", "application/json")
		if csrf := jar["passport_csrf_token"]; csrf != "" {
			h.Set("X-Tt-Passport-Csrf-Token", csrf)
		}
		h.Set("X-Tt-Passport-Trace-Id", randomHex(4))
	}
	return h
}

func randomHex(size int) string {
	raw := make([]byte, size)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

func (c *Client) commonQuery(acc *store.Account, extra url.Values) string {
	deviceID, webID, teaUUID, _ := identityFor(acc)
	q := url.Values{
		"version_code": {"20800"}, "language": {"zh"}, "device_platform": {"web"}, "doubao_device_platform": {"web"},
		"aid": {"497858"}, "real_aid": {"497858"}, "pkg_type": {"release_version"}, "device_id": {deviceID},
		"pc_version": {"3.44.7"}, "doubao_pc_version": {"3.44.7"}, "web_id": {webID}, "tea_uuid": {teaUUID},
		"region": {"CN"}, "sys_region": {"CN"}, "samantha_web": {"1"}, "web_platform": {"browser"},
		"use-olympus-account": {"1"}, "web_tab_id": {randomUUID()}, "tz_name": {"Asia/Shanghai"},
	}
	for key, values := range extra {
		q[key] = append([]string(nil), values...)
	}
	return q.Encode()
}

func randomUUID() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
}

func (c *Client) do(ctx context.Context, method, target string, body []byte, jar cookieJar, passport bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = c.headers(jar, passport)
	if len(body) > 0 && !passport {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil || response == nil {
		return response, err
	}
	// The browser-like transport deliberately exposes compressed bytes when a
	// caller supplied Accept-Encoding. Doubao commonly returns passport JSON as
	// Brotli, so decode it before any endpoint attempts JSON parsing.
	if err := httpclient.DecodeContentEncoding(response); err != nil {
		_ = response.Body.Close()
		return nil, fmt.Errorf("decode doubao response: %w", err)
	}
	return response, nil
}

type AccountInfo struct{ UserID, Name, Avatar string }

func (c *Client) AccountInfo(ctx context.Context, jar cookieJar) (AccountInfo, error) {
	q := "aid=497858&account_sdk_source=web&sdk_version=2.2.11-doubao.0&device_platform=web&language=zh"
	response, err := c.do(ctx, http.MethodGet, c.baseURL()+"/passport/account/info/v2/?"+q, nil, jar, false)
	if err != nil {
		return AccountInfo{}, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode >= 400 {
		return AccountInfo{}, statusError(response.StatusCode, raw)
	}
	var envelope struct {
		Data struct {
			UserID      json.RawMessage `json:"user_id"`
			UserIDStr   string          `json:"user_id_str"`
			Name        string          `json:"name"`
			ScreenName  string          `json:"screen_name"`
			Avatar      string          `json:"avatar_url"`
			ErrorCode   any             `json:"error_code"`
			Description string          `json:"description"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return AccountInfo{}, fmt.Errorf("doubao account response is not JSON")
	}
	if len(envelope.Data.UserID) == 0 {
		_ = json.Unmarshal(raw, &envelope.Data)
	}
	id := strings.TrimSpace(envelope.Data.UserIDStr)
	if id == "" {
		_ = json.Unmarshal(envelope.Data.UserID, &id)
	}
	if id == "" {
		return AccountInfo{}, fmt.Errorf("status=401: doubao cookie was rejected")
	}
	return AccountInfo{UserID: id, Name: first(envelope.Data.Name, envelope.Data.ScreenName), Avatar: envelope.Data.Avatar}, nil
}

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (c *Client) createConversation(ctx context.Context, jar cookieJar, source string) (string, error) {
	body := map[string]any{"cmd": 1100, "uplink_body": map[string]any{"create_conv_uplink_body": map[string]any{"conversation_type": 3, "participant_list": []map[string]any{{"user_id": "7338286299411103781", "user_type": 2}}}}, "sequence_id": strconv.FormatInt(time.Now().UnixMilli(), 10), "channel": 2, "version": "1"}
	if source != "" {
		body["uplink_body"].(map[string]any)["create_conv_uplink_body"].(map[string]any)["source_conversation_id"] = source
	}
	raw, _ := json.Marshal(body)
	response, err := c.do(ctx, http.MethodPost, c.baseURL()+"/im/conversation/create?"+c.commonQuery(c.acc, nil), raw, jar, false)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode >= 400 {
		return "", statusError(response.StatusCode, answer)
	}
	var envelope struct {
		StatusCode int    `json:"status_code"`
		StatusDesc string `json:"status_desc"`
		Downlink   struct {
			Create struct {
				Info struct {
					ID string `json:"conversation_id"`
				} `json:"conversation_info"`
			} `json:"create_conv_downlink_body"`
		} `json:"downlink_body"`
	}
	if json.Unmarshal(answer, &envelope) != nil || envelope.StatusCode != 0 || envelope.Downlink.Create.Info.ID == "" {
		return "", fmt.Errorf("doubao conversation creation failed: %s", envelope.StatusDesc)
	}
	return envelope.Downlink.Create.Info.ID, nil
}

func (c *Client) newConversation(ctx context.Context, jar cookieJar) (string, error) {
	root, err := c.createConversation(ctx, jar, "")
	if err != nil {
		return "", err
	}
	return c.createConversation(ctx, jar, root)
}

func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if c == nil || c.acc == nil {
		return fmt.Errorf("doubao client is missing an account")
	}
	if _, ok := FindModel(req.Model); !ok {
		return fmt.Errorf("doubao model %q is not supported", req.Model)
	}
	if err := validateTextOnly(req); err != nil {
		return err
	}
	if err := waitTurn(ctx, c.acc.ID, c.cfg); err != nil {
		return err
	}
	jar := c.jar()
	if len(jar) == 0 {
		return fmt.Errorf("status=401: doubao account is missing a cookie")
	}
	conversationID, err := c.newConversation(ctx, jar)
	if err != nil {
		return err
	}
	promptText := renderPrompt(req)
	toolMode := len(req.Tools) > 0 && !req.NoTools
	if toolMode {
		promptText += toolInstruction(req.Tools)
	}
	body, err := chatBody(conversationID, promptText, req.Model)
	if err != nil {
		return err
	}
	if logger != nil && !logger.Capturing() {
		logger.LogUpstreamRequest(c.baseURL()+"/chat/completion", map[string]string{"provider": "doubao", "model": req.Model}, body)
	}
	response, err := c.do(ctx, http.MethodPost, c.baseURL()+"/chat/completion?"+c.commonQuery(c.acc, nil), body, jar, false)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		return statusError(response.StatusCode, raw)
	}
	text, err := consumeStream(response.Body, func(delta string) {
		if !toolMode && onMessage != nil {
			onMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": delta}})
		}
	})
	if err != nil {
		return err
	}
	if toolMode {
		emitToolOrText(text, req.Tools, onMessage)
	}
	if onMessage != nil {
		onMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]any{"finishReason": "stop"}})
	}
	return nil
}

func validateTextOnly(req upstream.UpstreamRequest) error {
	for _, message := range req.Messages {
		for _, block := range message.Content.GetBlocks() {
			if block.Type != "" && block.Type != "text" && block.Type != "tool_result" && block.Type != "tool_use" {
				return fmt.Errorf("doubao currently supports text-only inputs")
			}
		}
	}
	if _, strict := req.StrictSchema(); strict && len(req.Tools) > 0 {
		return fmt.Errorf("doubao does not support tools with response_format")
	}
	return req.ValidateProtocolControls("doubao")
}

func renderPrompt(req upstream.UpstreamRequest) string {
	var out strings.Builder
	for _, item := range req.System {
		if text := strings.TrimSpace(item.Text); text != "" {
			fmt.Fprintf(&out, "[system]\n%s\n\n", text)
		}
	}
	for _, message := range req.Messages {
		text := strings.TrimSpace(message.ExtractText())
		if text == "" {
			continue
		}
		role := strings.TrimSpace(message.Role)
		if role == "developer" {
			role = "system"
		}
		fmt.Fprintf(&out, "[%s]\n%s\n\n", role, text)
	}
	return strings.TrimSpace(out.String())
}

func chatBody(conversationID, text, modelID string) ([]byte, error) {
	model, ok := FindModel(modelID)
	if !ok {
		return nil, fmt.Errorf("unknown doubao model")
	}
	now := time.Now().UnixMilli()
	task := map[string]bool{"server_do_compare": true}
	ack := map[string]bool{"need_ack_conversation": true}
	body := map[string]any{"client_meta": map[string]any{"local_conversation_id": fmt.Sprintf("local_%d", now), "conversation_id": conversationID, "bot_id": "7338286299411103781", "last_section_id": "", "last_message_index": nil, "local_permissions": []map[string]any{{"permission_name": "ACCESS_COARSE_LOCATION", "status": 3}, {"permission_name": "ACCESS_FINE_LOCATION", "status": 3}, {"permission_name": "ACCESS_BACKGROUND_LOCATION", "status": 3}}}, "messages": []any{map[string]any{"local_message_id": randomUUID(), "content_block": []any{map[string]any{"block_type": 10000, "content": map[string]any{"text_block": map[string]any{"text": text, "icon_url": "", "icon_url_dark": "", "summary": ""}, "pc_event_block": ""}, "block_id": randomUUID(), "parent_id": "", "meta_info": []any{}, "extra_info": map[string]any{}, "append_fields": []any{}}}, "message_status": 0}}, "option": map[string]any{"create_time_ms": now, "agent_mode": model.AgentMode, "need_create_conversation": false, "conversation_init_option": ack, "conversation_init_ext": map[string]any{"model_item_key": model.ModelItemKey, "reasoning_effort": strconv.Itoa(model.ReasoningEffort), "mode_id": model.ModeID}, "sse_recv_event_options": map[string]bool{"support_chunk_delta": true}, "support_lazy_fetch_stream": true, "general_task_param": task, "model_config": map[string]any{"model_item_key": model.ModelItemKey, "model_extra_params": map[string]any{}, "reasoning_effort": model.ReasoningEffort}, "aggregate_params": map[string]string{"conversation_mode": model.ConversationMode, "mode_id": model.ModeID, "model_item_key": model.ModelItemKey, "agent_mode": strconv.Itoa(model.AgentMode), "reasoning_effort": strconv.Itoa(model.ReasoningEffort)}, "conversation_mode": atoi(model.ConversationMode)}, "user_context": []any{}, "ext": map[string]string{"agent_mode": strconv.Itoa(model.AgentMode), "use_deep_think": "0", "sub_conv_firstmet_type": "1", "general_task_param": `{"server_do_compare":true}`, "collection_id": "", "is_finish": "1", "conversation_init_option": `{"need_ack_conversation":true}`, "commerce_credit_config_enable": "0"}}
	return json.Marshal(body)
}

func atoi(value string) int { parsed, _ := strconv.Atoi(value); return parsed }

func consumeStream(body io.Reader, emit func(string)) (string, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var event, data, userMessageID string
	var text, snapshot string
	flush := func() error {
		if event == "" {
			data = ""
			return nil
		}
		var value map[string]any
		if data != "" && json.Unmarshal([]byte(data), &value) == nil {
			switch event {
			case "FULL_MSG_NOTIFY":
				if message, ok := value["message"].(map[string]any); ok {
					userMessageID, _ = message["message_id"].(string)
				}
			case "CHUNK_DELTA":
				if delta, _ := value["text"].(string); delta != "" {
					text += delta
					emit(delta)
				}
			case "STREAM_ERROR":
				return upstreamStreamError(value)
			case "STREAM_CHUNK", "STREAM_MSG_NOTIFY", "MSG_NOTIFY":
				messageID, _ := value["message_id"].(string)
				if messageID == "" {
					if message, ok := value["message"].(map[string]any); ok {
						messageID, _ = message["message_id"].(string)
					}
				}
				if messageID != "" && messageID == userMessageID {
					break
				}
				if candidate := snapshotText(value); len(candidate) > len(snapshot) {
					snapshot = candidate
				}
			}
		}
		event, data = "", ""
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return "", err
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(value)
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data += strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if err := flush(); err != nil {
		return "", err
	}
	if text == "" {
		text = snapshot
		if text != "" {
			emit(text)
		}
	}
	return text, nil
}

func snapshotText(value map[string]any) string {
	raw, _ := json.Marshal(value)
	var walk func(any) string
	walk = func(node any) string {
		switch typed := node.(type) {
		case map[string]any:
			if block, ok := typed["text_block"].(map[string]any); ok {
				if text, _ := block["text"].(string); text != "" {
					return text
				}
			}
			for _, child := range typed {
				if found := walk(child); found != "" {
					return found
				}
			}
		case []any:
			for _, child := range typed {
				if found := walk(child); found != "" {
					return found
				}
			}
		}
		return ""
	}
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	return walk(decoded)
}

func upstreamStreamError(value map[string]any) error {
	code := fmt.Sprint(value["error_code"])
	if code == "710022002" || code == "710022004" {
		return fmt.Errorf("status=429: doubao upstream rate limited (%s)", code)
	}
	return fmt.Errorf("doubao stream error: %s", code)
}

func statusError(status int, raw []byte) error {
	text := strings.TrimSpace(string(raw))
	if strings.Contains(text, "710022002") || strings.Contains(text, "710022004") || status == 429 {
		return fmt.Errorf("status=429: doubao upstream rate limited")
	}
	if status == 401 || status == 403 || strings.Contains(strings.ToLower(text), "login") {
		return fmt.Errorf("status=401: doubao cookie was rejected")
	}
	return fmt.Errorf("status=%d: doubao upstream error", status)
}

var turns = struct {
	sync.Mutex
	last map[int64]time.Time
}{last: map[int64]time.Time{}}

func waitTurn(ctx context.Context, accountID int64, cfg *config.Config) error {
	gap := 3 * time.Second
	if cfg != nil && cfg.DoubaoRequestGapMS > 0 {
		gap = time.Duration(cfg.DoubaoRequestGapMS) * time.Millisecond
	}
	turns.Lock()
	next := turns.last[accountID].Add(gap)
	now := time.Now()
	if next.Before(now) {
		next = now
	}
	turns.last[accountID] = next
	turns.Unlock()
	if wait := time.Until(next); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func toolInstruction(tools []any) string {
	raw, _ := json.Marshal(tools)
	return "\n\nYou must answer using exactly one JSON object. For a normal answer return {\"content\":\"your answer\"}. To call a function return {\"tool_calls\":[{\"name\":\"declared function name\",\"arguments\":{}}]}. Do not use markdown. Declared functions:\n" + string(raw)
}

func emitToolOrText(text string, tools []any, emit func(upstream.SSEMessage)) {
	if emit == nil {
		return
	}
	var envelope struct {
		Content   string `json:"content"`
		ToolCalls []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal([]byte(text), &envelope) == nil && len(envelope.ToolCalls) > 0 {
		allowed := map[string]bool{}
		for _, tool := range tools {
			row, _ := tool.(map[string]any)
			function, _ := row["function"].(map[string]any)
			if name, _ := function["name"].(string); name != "" {
				allowed[name] = true
			}
		}
		emitted := false
		for _, call := range envelope.ToolCalls {
			var args map[string]any
			if !allowed[call.Name] || json.Unmarshal(call.Arguments, &args) != nil || args == nil {
				continue
			}
			emit(upstream.SSEMessage{Type: "model.tool-call", Event: map[string]any{"toolCallId": "call_" + randomHex(8), "toolName": call.Name, "input": string(call.Arguments)}})
			emitted = true
		}
		if emitted {
			return
		}
	}
	if envelope.Content != "" {
		text = envelope.Content
	}
	emit(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": text}})
}
