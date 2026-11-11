package doubao

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	defaultUserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36"
	// botID is the assistant the web client addresses; it is fixed for the
	// public Doubao bot and appears in both the conversation and chat bodies.
	botID = "7338286299411103781"
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
	jar := decodeCookies(c.acc.ClientCookie)
	// The web client always carries its UI preferences; a QR session has them
	// only if it visited the page with JavaScript enabled. Seeding the static
	// ones keeps the cookie set from looking like a headless client.
	for name, value := range map[string]string{
		"i18next":           "zh",
		"dbx-web-theme":     "light",
		"flow_user_country": "CN",
	} {
		if _, ok := jar[name]; !ok {
			jar[name] = value
		}
	}
	return jar
}

func (c *Client) headers(jar cookieJar, passport bool) http.Header {
	h := http.Header{}
	h.Set("User-Agent", c.userAgent())
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9")
	// The current web client advertises zstd as well. Omitting it is a cheap
	// fingerprint difference, so match the capture.
	h.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	h.Set("Referer", c.baseURL()+"/chat/")
	h.Set("Origin", c.baseURL())
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	major := "157"
	if match := chromeVersion.FindStringSubmatch(c.userAgent()); len(match) == 2 {
		major = match[1]
	}
	// Brand order and the GREASE brand match the captured request.
	h.Set("Sec-Ch-Ua", `"Not-A.Brand";v="99", "Google Chrome";v="`+major+`", "Chromium";v="`+major+`"`)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	h.Set("X-Flow-Trace", "04-"+randomHex(16)+"-"+randomHex(8)+"-01")
	h.Set("Agw-Js-Conv", "str, str")
	// Only passport endpoints reject the CSRF cookie (2035). The captured chat
	// request carries it, so send the full jar everywhere else.
	if cookie := jar.header(passport); cookie != "" {
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
	deviceID, webID, teaUUID := identityFor(acc)
	q := url.Values{
		"version_code": {"20800"}, "language": {"zh"}, "device_platform": {"web"}, "doubao_device_platform": {"web"},
		"aid": {"497858"}, "real_aid": {"497858"}, "pkg_type": {"release_version"}, "device_id": {deviceID},
		"pc_version": {"3.45.1"}, "doubao_pc_version": {"3.45.1"}, "web_id": {webID}, "tea_uuid": {teaUUID},
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

// fingerprintQuery mirrors the web client: a jar created from a browser export
// carries s_v_web_id, and the request must present the same value as fp or the
// gateway treats the session and the request as coming from different devices.
func fingerprintQuery(jar cookieJar) url.Values {
	if fp := strings.TrimSpace(jar["s_v_web_id"]); fp != "" {
		return url.Values{"fp": {fp}}
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, target string, body []byte, jar cookieJar, passport bool) (*http.Response, error) {
	return c.doJSON(ctx, method, target, body, jar, passport, "application/json", "")
}

// doJSON sends a request with an explicit JSON content type.
//
// The IM gateway inspects the content type and rejects application/json with
// status 712012002 "unsupported encoding type". The web client sends
// `application/json; encoding=utf-8` for /im/* calls while /chat/completion
// keeps plain application/json, so the value cannot be set globally.
func (c *Client) doJSON(ctx context.Context, method, target string, body []byte, jar cookieJar, passport bool, contentType, accept string) (*http.Response, error) {
	return c.doJSONWithReferer(ctx, method, target, body, jar, passport, contentType, accept, "")
}

// doJSONWithReferer lets the streaming call point Referer at the local
// conversation the body declares, which is what the web client does.
func (c *Client) doJSONWithReferer(ctx context.Context, method, target string, body []byte, jar cookieJar, passport bool, contentType, accept, referer string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// Passport endpoints reject the CSRF cookie with 2035 wherever they are
	// called from, so decide by target as well as by the caller's profile.
	req.Header = c.headers(jar, passport || strings.Contains(target, "/passport/"))
	if len(body) > 0 && !passport {
		req.Header.Set("Content-Type", contentType)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
		// The web client's EventSource helper sends this literal on chat streams.
		req.Header.Set("Last-Event-ID", "undefined")
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

type AccountInfo struct{ UserID, Name string }

// apply writes the observed identity onto an account and stamps the
// verification time. Every caller that refreshes an account needs exactly this,
// so the rule for "when is an account considered verified" lives in one place.
func (info AccountInfo) apply(target *store.Account, now time.Time) bool {
	if target == nil {
		return false
	}
	target.UserID = info.UserID
	if info.Name != "" {
		target.Name = info.Name
	}
	target.DoubaoVerifiedAt = now
	target.VerifiedAt = now
	return true
}

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
			UserID     json.RawMessage `json:"user_id"`
			UserIDStr  string          `json:"user_id_str"`
			Name       string          `json:"name"`
			ScreenName string          `json:"screen_name"`
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
	return AccountInfo{UserID: id, Name: first(envelope.Data.Name, envelope.Data.ScreenName)}, nil
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
	body := map[string]any{"cmd": 1100, "uplink_body": map[string]any{"create_conv_uplink_body": map[string]any{"conversation_type": 3, "participant_list": []map[string]any{{"user_id": botID, "user_type": 2}}}}, "sequence_id": strconv.FormatInt(time.Now().UnixMilli(), 10), "channel": 2, "version": "1"}
	if source != "" {
		body["uplink_body"].(map[string]any)["create_conv_uplink_body"].(map[string]any)["source_conversation_id"] = source
	}
	raw, _ := json.Marshal(body)
	query := c.commonQuery(c.acc, fingerprintQuery(jar))
	response, err := c.doJSON(ctx, http.MethodPost, c.baseURL()+"/im/conversation/create?"+query, raw, jar, false, "application/json; encoding=utf-8", "")
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
	if err := json.Unmarshal(answer, &envelope); err != nil {
		return "", fmt.Errorf("doubao conversation creation returned non-JSON (status=%d): %s", response.StatusCode, compactError(string(answer)))
	}
	if envelope.StatusCode != 0 || envelope.Downlink.Create.Info.ID == "" {
		return "", fmt.Errorf("doubao conversation creation failed: status_code=%d status_desc=%s", envelope.StatusCode, compactError(envelope.StatusDesc))
	}
	return envelope.Downlink.Create.Info.ID, nil
}

// conversationFor returns the upstream conversation that carries one downstream
// conversation, creating it on first use.
//
// Isolation is keyed by the caller's conversation id rather than the account.
// Sharing one upstream conversation per account let one client's history leak
// into another client's answer and grew a single conversation without bound.
// Requests without a conversation id fall back to a per-account conversation, so
// a client that sends no id still reuses one instead of minting a new session
// per turn.
func (c *Client) conversationFor(ctx context.Context, jar cookieJar, downstreamID string) (string, error) {
	account := c.acc.ID
	root, err := c.mainConversation(ctx, jar)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(downstreamID) == "" {
		// Without a downstream id there is nothing to isolate: keep using the
		// account's root so a turn does not mint a new upstream session.
		return root, nil
	}
	key := cacheKey(account, downstreamID)
	if key != "" {
		conversations.Lock()
		cached := conversations.byConversation[key]
		conversations.Unlock()
		if cached != "" {
			return cached, nil
		}
	}
	// Creating with a source id opens a *child* conversation. Creating without
	// one returns the account's root and is idempotent, so it can never isolate
	// two downstream conversations.
	id, err := c.createConversation(ctx, jar, root)
	if err != nil {
		return "", err
	}
	slog.Debug("doubao conversation selected", "account_id", account, "downstream", downstreamID, "root", root, "conversation", id)
	if key != "" {
		conversations.Lock()
		conversations.byConversation[key] = id
		conversations.Unlock()
	}
	return id, nil
}

// mainConversation returns the account's root conversation, creating it once.
func (c *Client) mainConversation(ctx context.Context, jar cookieJar) (string, error) {
	if c.acc.ID != 0 {
		conversations.Lock()
		root := conversations.main[c.acc.ID]
		conversations.Unlock()
		if root != "" {
			return root, nil
		}
	}
	root, err := c.createConversation(ctx, jar, "")
	if err != nil {
		return "", err
	}
	if c.acc.ID != 0 {
		conversations.Lock()
		conversations.main[c.acc.ID] = root
		conversations.Unlock()
	}
	return root, nil
}

// cacheKey scopes a conversation to an account and a downstream conversation.
// An unknown account disables caching rather than sharing one bucket.
func cacheKey(account int64, downstreamID string) string {
	if account == 0 {
		return ""
	}
	return fmt.Sprintf("%d|%s", account, strings.TrimSpace(downstreamID))
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
	conversationID, err := c.conversationFor(ctx, jar, req.ConversationID)
	if err != nil {
		return err
	}
	promptText := renderPrompt(req)
	promptText, err = c.expandPromptURLs(ctx, promptText)
	if err != nil {
		return err
	}
	toolMode := usesTools(req)
	if toolMode {
		promptText += toolInstruction(req.Tools, req.ToolChoice)
	} else {
		promptText += formatInstruction(req)
	}
	attachments, err := c.uploadRequestAttachments(ctx, jar, req)
	if err != nil {
		return err
	}
	localID := fmt.Sprintf("local_%d", time.Now().UnixMilli())
	body, err := chatBodyWithAttachments(conversationID, localID, promptText, req.Model, attachments)
	if err != nil {
		return err
	}
	if logger != nil && !logger.Capturing() {
		logger.LogUpstreamRequest(c.baseURL()+"/chat/completion", map[string]string{"provider": "doubao", "model": req.Model}, body)
	}
	query := c.commonQuery(c.acc, fingerprintQuery(jar))
	response, err := c.doJSONWithReferer(ctx, http.MethodPost, c.baseURL()+"/chat/completion?"+query, body, jar, false, "application/json", "text/event-stream", c.baseURL()+"/chat/"+localID)
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
	if text == "" {
		slog.Warn("doubao answer was empty", "account_id", c.acc.ID, "model", req.Model, "attachments", len(attachments))
	}
	finishReason := "stop"
	if toolMode && emitToolOrText(text, req.Tools, onMessage) {
		// A turn that ends in a call must say so: a client loops on the finish
		// reason, not on the presence of tool_calls.
		finishReason = "tool_use"
	}
	if onMessage != nil {
		onMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]any{"finishReason": finishReason}})
	}
	return nil
}

func validateTextOnly(req upstream.UpstreamRequest) error {
	for _, message := range req.Messages {
		for _, block := range message.Content.GetBlocks() {
			switch block.Type {
			case "", "text", "tool_result", "tool_use":
			case "image", "image_url", "input_image", "file", "input_file":
				// Uploaded as attachments below.
			default:
				return fmt.Errorf("doubao does not accept %q content", block.Type)
			}
		}
	}
	if _, strict := req.StrictSchema(); strict && len(req.Tools) > 0 {
		return fmt.Errorf("doubao does not support tools with response_format")
	}
	return req.ValidateProtocolControls("doubao")
}

func chatBody(conversationID, text, modelID string) ([]byte, error) {
	return chatBodyWithAttachments(conversationID, fmt.Sprintf("local_%d", time.Now().UnixMilli()), text, modelID, nil)
}

// chatBodyWithAttachments is chatBody plus the attachment messages that must
// precede the text block in the same request.
func chatBodyWithAttachments(conversationID, localID, text, modelID string, attachments []storeURI) ([]byte, error) {
	model, ok := FindModel(modelID)
	if !ok {
		return nil, fmt.Errorf("unknown doubao model")
	}
	now := time.Now().UnixMilli()
	effort, agent := strconv.Itoa(model.ReasoningEffort), strconv.Itoa(model.AgentMode)
	body := map[string]any{
		"client_meta": map[string]any{
			"local_conversation_id": localID, "conversation_id": conversationID,
			"bot_id": botID, "last_section_id": "", "last_message_index": nil,
			"local_permissions": []map[string]any{
				{"permission_name": "ACCESS_COARSE_LOCATION", "status": 3},
				{"permission_name": "ACCESS_FINE_LOCATION", "status": 3},
				{"permission_name": "ACCESS_BACKGROUND_LOCATION", "status": 3},
			},
		},
		"messages": append(attachmentMessages(attachments), textMessage(text)),
		"option": map[string]any{
			// The web client sends a fixed envelope; omitting the no-op fields
			// makes the request look hand-built. Attachment turns are the most
			// sensitive: the parser only reads an image when the surrounding
			// option block matches what the page sends.
			"send_message_scene": "", "create_time_ms": now, "collect_id": "",
			"is_audio": false, "answer_with_suggest": false, "agent_mode": model.AgentMode,
			"tts_switch": false, "need_deep_think": 0, "click_clear_context": false,
			"from_suggest": false, "is_regen": false, "is_replace": false,
			"is_from_click_option": false, "is_from_click_softlink": false,
			"disable_sse_cache": false, "select_text_action": "", "is_select_text": false,
			"resend_for_regen": false, "scene_type": 0, "unique_key": randomUUID(), "start_seq": 0,
			"need_create_conversation": false,
			"conversation_init_option": map[string]bool{"need_ack_conversation": true},
			"conversation_init_ext":    map[string]any{"model_item_key": model.ModelItemKey, "reasoning_effort": effort, "mode_id": model.ModeID},
			"regen_query_id":           []any{}, "edit_query_id": []any{}, "regen_instruction": "",
			"no_replace_for_regen": false, "message_from": 0,
			"shared_app_name": "", "shared_app_id": "",
			"sse_recv_event_options":    map[string]bool{"support_chunk_delta": true},
			"support_lazy_fetch_stream": true,
			"is_ai_playground":          false, "is_old_user": true,
			"general_task_param":   map[string]bool{"server_do_compare": true},
			"recovery_option":      map[string]any{"is_recovery": false, "req_create_time_sec": now / 1000, "append_sse_event_scene": 0},
			"message_storage_type": 0, "related_deleted_message_ids": map[string]any{},
			"connector_info_list": []any{},
			"model_config":        map[string]any{"model_item_key": model.ModelItemKey, "model_extra_params": map[string]any{}, "reasoning_effort": model.ReasoningEffort},
			"aggregate_params": map[string]string{
				"mention_skill_list": "[]", "mention_plugin_list": "[]", "mention_ext": "[{}]",
				"conversation_mode": model.ConversationMode, "mode_id": model.ModeID,
				"model_item_key": model.ModelItemKey, "agent_mode": agent,
				"reasoning_effort": effort, "provider_id": "",
			},
			"conversation_mode": atoi(model.ConversationMode),
		},
		"user_context": []any{},
		"ext": map[string]string{
			"agent_mode": agent, "use_deep_think": "0", "sub_conv_firstmet_type": "1",
			"general_task_param": `{"server_do_compare":true}`, "collection_id": "", "is_finish": "1",
			"conversation_init_option": `{"need_ack_conversation":true}`, "commerce_credit_config_enable": "0",
		},
	}
	return json.Marshal(body)
}

func textMessage(text string) map[string]any {
	return map[string]any{
		"local_message_id": randomUUID(), "message_status": 0,
		"content_block": []any{map[string]any{
			"block_type": 10000, "block_id": randomUUID(), "parent_id": "",
			"meta_info": []any{}, "extra_info": map[string]any{}, "append_fields": []any{},
			"content": map[string]any{"pc_event_block": "", "text_block": map[string]any{"text": text, "icon_url": "", "icon_url_dark": "", "summary": ""}},
		}},
	}
}

func atoi(value string) int { parsed, _ := strconv.Atoi(value); return parsed }

func statusError(status int, raw []byte) error {
	text := strings.TrimSpace(string(raw))
	// A challenge refused before the stream opened is still a challenge: it
	// must park the account, not be retried as a rate limit.
	if strings.Contains(text, "710022004") {
		if decision := decisionFromText(text); decision.needsVerification() {
			return &verificationRequired{code: "710022004", decision: decision}
		}
	}
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

var conversations = struct {
	sync.Mutex
	main           map[int64]string
	byConversation map[string]string
}{main: map[int64]string{}, byConversation: map[string]string{}}

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
