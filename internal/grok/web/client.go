package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/grok/web/egress"
	"orchids-api/internal/httpclient"
)

const (
	defaultBaseURL          = "https://grok.com"
	defaultAssetsBaseURL    = "https://assets.grok.com"
	defaultAssetsListPath   = "/rest/assets"
	defaultAssetsDeletePath = "/rest/assets-metadata"
	defaultChatPath         = "/rest/app-chat/conversations/new"
	defaultConversationPath = "/rest/app-chat/conversations"
	defaultUploadFilePath   = "/rest/app-chat/upload-file"
	defaultCanvasCreatePath = "/rest/media/canvas/create"
	defaultMediaConvoPath   = "/rest/media/conversation/create"
	defaultLivekitPath      = "/rest/livekit/tokens"
	defaultRateLimitsPath   = "/rest/rate-limits"
	defaultAcceptTOSURL     = "https://accounts.x.ai/auth_mgmt.AuthManagement/SetTosAcceptedVersion"
	defaultSetBirthPath     = "/rest/auth/set-birth-date"
	defaultNSFWMgmtPath     = "/auth_mgmt.AuthManagement/UpdateUserFeatureControls"
	defaultUA               = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
	defaultAppChatSecCHUA   = `"Chromium";v="148", "Google Chrome";v="148", "Not/A)Brand";v="99"`
)

type Client struct {
	cfg         *config.Config
	httpClient  *http.Client
	assetClient *http.Client
	egress      *egress.Manager
	// appChatTransport is deliberately kept behind a small boundary.  The
	// historical REST request shape and the newer MGW websocket request shape
	// are not wire-compatible, so callers must never silently reinterpret an
	// app-chat payload as an MGW event stream.
	appChatTransport appChatTransport
}

type NSFWEnableResult struct {
	Success     bool   `json:"success"`
	HTTPStatus  int    `json:"http_status"`
	GRPCStatus  int    `json:"grpc_status,omitempty"`
	GRPCMessage string `json:"grpc_message,omitempty"`
	Error       string `json:"error,omitempty"`
}

func New(cfg *config.Config) *Client {
	cfg = cfg.GrokWebTransport()
	timeout := cfg.GrokWebRequestTimeout()
	baseClient := newHTTPClient(cfg, timeout, httpclient.ProxyFuncFromConfig(cfg))
	client := &Client{
		cfg:         cfg,
		httpClient:  baseClient,
		assetClient: baseClient,
		egress:      egress.Shared(cfg),
	}
	// MGW is available through the explicit request API in
	// mgw_websocket_transport.go.  Legacy callers only provide a REST payload,
	// therefore the adapter records that MGW is not applicable and safely falls
	// back to the established REST transport.
	client.appChatTransport = newAppChatFallbackTransport(client)
	return client
}

func (c *Client) baseURL() string {
	if c.cfg != nil {
		return c.cfg.GrokWebBaseURLOrDefault()
	}
	return defaultBaseURL
}

func (c *Client) userAgent() string {
	if c.cfg != nil {
		return c.cfg.GrokWebUserAgentOrDefault()
	}
	return defaultUA
}

func (c *Client) chatPayload(spec ModelSpec, text string, noMemory bool, imageCount int) map[string]interface{} {
	cnt := imageCount
	if cnt <= 0 {
		cnt = 2
	}
	temporary := true
	if c != nil && c.cfg != nil {
		temporary = c.cfg.GrokChatTemporary()
		noMemory = c.cfg.GrokChatDisableMemory(noMemory)
	}
	includeModelFields := spec.IsImage || spec.IsVideo
	responseMetadata := map[string]interface{}{}
	if includeModelFields {
		responseMetadata["modelConfigOverride"] = map[string]interface{}{
			"modelMap": map[string]interface{}{},
		}
		responseMetadata["requestModelDetails"] = map[string]interface{}{
			"modelId": spec.UpstreamModel,
		}
	}
	payload := map[string]interface{}{
		"collectionIds":               []string{},
		"disabledConnectorIds":        []string{},
		"temporary":                   temporary,
		"message":                     text,
		"fileAttachments":             []string{},
		"imageAttachments":            []string{},
		"disableSearch":               false,
		"enableImageGeneration":       true,
		"returnImageBytes":            false,
		"enableImageStreaming":        true,
		"imageGenerationCount":        cnt,
		"forceConcise":                false,
		"forceSideBySide":             false,
		"isAsyncChat":                 false,
		"linkQuery":                   false,
		"isReasoning":                 false,
		"disableSelfHarmShortCircuit": false,
		"disableTextFollowUps":        false,
		"returnRawGrokInXaiRequest":   false,
		"enableSideBySide":            true,
		"sendFinalMetadata":           true,
		"responseMetadata":            responseMetadata,
		"disableMemory":               noMemory,
		"deviceEnvInfo":               appChatDeviceEnvInfo(),
	}
	if includeModelFields {
		payload["modelName"] = spec.UpstreamModel
	}
	if imageCount > 0 {
		payload["toolOverrides"] = map[string]interface{}{"imageGen": true}
	}
	if includeModelFields && strings.TrimSpace(spec.ModelMode) != "" {
		payload["modelMode"] = spec.ModelMode
	}
	if modeID := appChatModeID(spec); modeID != "" {
		payload["modeId"] = modeID
	}
	if includeModelFields {
		if tier := appChatModelTier(spec); tier != "" {
			payload["modelTier"] = tier
		}
	}
	if includeModelFields && spec.PreferBest {
		payload["preferBest"] = true
	}
	if c != nil && c.cfg != nil {
		if customPersonality := c.cfg.GrokChatCustomInstruction(); customPersonality != "" {
			payload["customPersonality"] = customPersonality
		}
	}
	return payload
}

func (c *Client) appChatImagePayload(spec ModelSpec, prompt, _ string, _ int) map[string]interface{} {
	message := strings.TrimSpace(prompt)
	if message != "" && !strings.HasPrefix(strings.ToLower(message), "drawing:") {
		message = "Drawing: " + message
	}
	// grok.com refuses image-model fields (modelName grok-imagine-image-lite,
	// modelTier, requestModelDetails) for Free accounts with "Grok is under
	// heavy usage"; a plain fast-mode chat with image generation enabled is
	// what the web app sends, and it draws from the fast quota every plan has.
	_ = spec
	chat := ModelSpec{ID: "grok-chat-fast", UpstreamModel: "grok-chat-fast", ModelMode: "MODEL_MODE_FAST", ModeID: "fast", Tier: grokTierBasic}
	payload := c.chatPayload(chat, message, false, 2)
	payload["modeId"] = "fast"
	payload["enableImageGeneration"] = true
	payload["imageGenerationCount"] = 2
	return payload
}

func appChatModeID(spec ModelSpec) string {
	return resolveModeID(spec, strings.TrimSpace(spec.ID))
}

// resolveModeID maps ModelSpec mode fields to the upstream mode/model name.
// fallback is used when ModeID, ModelMode, and UpstreamModel are all empty
// (app-chat falls back to the public model ID; rate-limit probes fall back to "fast").
func resolveModeID(spec ModelSpec, fallback string) string {
	if modeID := strings.TrimSpace(spec.ModeID); modeID != "" {
		return modeID
	}
	mode := strings.TrimSpace(spec.ModelMode)
	switch mode {
	case "MODEL_MODE_FAST":
		return "fast"
	case "MODEL_MODE_AUTO":
		return "auto"
	case "MODEL_MODE_EXPERT":
		return "expert"
	case "MODEL_MODE_HEAVY":
		return "heavy"
	case "MODEL_MODE_GROK_4_3":
		return "grok-420-computer-use-sa"
	}
	if mode != "" {
		return mode
	}
	if upstream := strings.TrimSpace(spec.UpstreamModel); upstream != "" {
		return upstream
	}
	return fallback
}

func appChatModelTier(spec ModelSpec) string {
	switch spec.Tier {
	case grokTierBasic:
		return "basic"
	case grokTierLite:
		return "lite"
	case grokTierSuper:
		return "super"
	case grokTierHeavy:
		return "heavy"
	default:
		return ""
	}
}

func (c *Client) clientForAsset(asset bool) *http.Client {
	if asset && c.assetClient != nil {
		return c.assetClient
	}
	return c.httpClient
}

func (c *Client) clientForAppChat() *http.Client {
	if c != nil && c.httpClient != nil {
		return c.httpClient
	}
	return http.DefaultClient
}

func (c *Client) doChat(ctx context.Context, token string, payload map[string]interface{}) (*http.Response, error) {
	if c == nil {
		return nil, fmt.Errorf("grok client not configured")
	}
	transport := c.appChatTransport
	if transport == nil {
		transport = restAppChatTransport{}
	}
	return transport.Chat(ctx, c, token, payload)
}

// doRESTChat preserves the proven REST app-chat transport.  It is intentionally
// separate from doChat so the transport selector can fall back without
// recursively calling itself.
func (c *Client) doRESTChat(ctx context.Context, token string, payload map[string]interface{}) (*http.Response, error) {
	if err := waitScopedRateLimit(ctx, ProviderWeb, token, parseLooseStringAny(payload["modelName"]), c.cfg.GrokWebRequestsPerSecond()); err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqURL := c.baseURL() + defaultChatPath
	headers := c.appChatHeadersFor(ctx, token, http.MethodPost, reqURL)
	if media, _ := payload["mediaGenInput"].(map[string]interface{}); media["imageToImage"] != nil {
		headers.Set("Referer", c.baseURL()+"/imagine")
	}
	return c.doAppChatRequest(ctx, reqURL, body, headers)
}

func (c *Client) doAppChatCreateAndRespond(ctx context.Context, token string, payload map[string]interface{}) (*http.Response, error) {
	canvasID, err := c.createAppChatCanvas(ctx, token)
	if err != nil {
		return nil, err
	}
	referer := c.appChatImagineReferer(canvasID)
	conversationID, err := c.createAppChatConversation(ctx, token, referer)
	if err != nil {
		return nil, err
	}
	if err := c.linkAppChatConversationToCanvas(ctx, token, conversationID, canvasID); err != nil {
		return nil, err
	}
	return c.doAppChatAddResponse(ctx, token, conversationID, canvasID, payload)
}

func (c *Client) appChatImagineReferer(canvasID string) string {
	id := strings.TrimSpace(canvasID)
	if id == "" {
		return c.baseURL() + "/imagine/agent"
	}
	return c.baseURL() + "/imagine/agent/" + url.PathEscape(id)
}

func (c *Client) createAppChatCanvas(ctx context.Context, token string) (string, error) {
	body, err := json.Marshal(map[string]interface{}{
		"name": "",
	})
	if err != nil {
		return "", err
	}
	reqURL := c.baseURL() + defaultCanvasCreatePath
	headers := c.appChatHeadersWithReferer(token, c.appChatImagineReferer(""))
	if statsig := c.statsigIDForRequest(ctx, token, http.MethodPost, reqURL); statsig != "" {
		headers.Set("x-statsig-id", statsig)
	}
	resp, err := c.doAppChatRequest(ctx, reqURL, body, headers)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var payload map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("grok app-chat create canvas decode failed: %w", err)
	}
	canvasID := extractAppChatCanvasID(payload)
	if canvasID == "" {
		return "", fmt.Errorf("grok app-chat create canvas returned no canvas id")
	}
	return canvasID, nil
}

func (c *Client) createAppChatConversation(ctx context.Context, token, referer string) (string, error) {
	body, err := json.Marshal(map[string]interface{}{
		"systemPromptName": "",
		"temporary":        false,
	})
	if err != nil {
		return "", err
	}
	reqURL := c.baseURL() + defaultConversationPath
	headers := c.appChatHeadersWithReferer(token, referer)
	if statsig := c.statsigIDForRequest(ctx, token, http.MethodPost, reqURL); statsig != "" {
		headers.Set("x-statsig-id", statsig)
	}
	resp, err := c.doAppChatRequest(ctx, reqURL, body, headers)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var payload map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("grok app-chat create conversation decode failed: %w", err)
	}
	conversationID := extractAppChatConversationID(payload)
	if conversationID == "" {
		return "", fmt.Errorf("grok app-chat create conversation returned no conversation id")
	}
	return conversationID, nil
}

func (c *Client) linkAppChatConversationToCanvas(ctx context.Context, token, conversationID, canvasID string) error {
	conversationID = strings.TrimSpace(conversationID)
	canvasID = strings.TrimSpace(canvasID)
	if conversationID == "" {
		return fmt.Errorf("empty app-chat conversation id")
	}
	if canvasID == "" {
		return fmt.Errorf("empty app-chat canvas id")
	}
	body, err := json.Marshal(map[string]interface{}{
		"conversationId": conversationID,
		"canvasId":       canvasID,
	})
	if err != nil {
		return err
	}
	reqURL := c.baseURL() + defaultMediaConvoPath
	resp, err := c.doAppChatRequest(ctx, reqURL, body, c.appChatHeadersWithReferer(token, c.appChatImagineReferer(canvasID)))
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func (c *Client) doAppChatAddResponse(ctx context.Context, token, conversationID, canvasID string, payload map[string]interface{}) (*http.Response, error) {
	id := strings.TrimSpace(conversationID)
	if id == "" {
		return nil, fmt.Errorf("empty app-chat conversation id")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	reqURL := c.baseURL() + defaultConversationPath + "/" + url.PathEscape(id) + "/responses"
	return c.doAppChatRequest(ctx, reqURL, body, c.appChatHeadersWithReferer(token, c.appChatImagineReferer(canvasID)))
}
