package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"encoding/json"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/config"
	"orchids-api/internal/grok/web/egress"
	"orchids-api/internal/httpclient"
	"orchids-api/internal/upstream"
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

func (c *Client) chatPayload(spec ModelSpec, text string, noMemory bool, imageCount int) map[string]any {
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
	responseMetadata := map[string]any{}
	if includeModelFields {
		responseMetadata["modelConfigOverride"] = map[string]any{
			"modelMap": map[string]any{},
		}
		responseMetadata["requestModelDetails"] = map[string]any{
			"modelId": spec.UpstreamModel,
		}
	}
	payload := map[string]any{
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
		payload["toolOverrides"] = map[string]any{"imageGen": true}
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

func (c *Client) doChat(ctx context.Context, token string, payload map[string]any) (*http.Response, error) {
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
func (c *Client) doRESTChat(ctx context.Context, token string, payload map[string]any) (*http.Response, error) {
	if err := waitScopedRateLimit(ctx, ProviderWeb, token, chatwire.ParseLooseStringAny(payload["modelName"]), c.cfg.GrokWebRequestsPerSecond()); err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqURL := c.baseURL() + defaultChatPath
	headers := c.appChatHeadersFor(ctx, token, http.MethodPost, reqURL)
	if media, _ := payload["mediaGenInput"].(map[string]any); media["imageToImage"] != nil {
		headers.Set("Referer", c.baseURL()+"/imagine")
	}
	// grok.com does not name a model on its answer, so only the sent mode is
	// recorded for the journal.
	sent := chatwire.ParseLooseStringAny(payload["modelName"])
	if sent == "" {
		sent = chatwire.ParseLooseStringAny(payload["modeId"])
	}
	upstream.NoteUpstreamCall(ctx, reqURL, sent)
	return c.doAppChatRequest(ctx, reqURL, body, headers)
}
