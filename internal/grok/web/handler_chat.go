package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/httpclient"
	"orchids-api/internal/logutil"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// maxChatAttachments bounds how many media inputs one request may reference.
const maxChatAttachments = 8

func appendUsage(dst []byte, usage map[string]interface{}) []byte {
	if len(usage) == 0 {
		dst = append(dst, `,"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":0,"text_tokens":0,"audio_tokens":0,"image_tokens":0},"completion_tokens_details":{"text_tokens":0,"audio_tokens":0,"reasoning_tokens":0}}`...)
		return dst
	}
	raw, err := json.Marshal(usage)
	if err != nil {
		dst = append(dst, `,"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":0,"text_tokens":0,"audio_tokens":0,"image_tokens":0},"completion_tokens_details":{"text_tokens":0,"audio_tokens":0,"reasoning_tokens":0}}`...)
		return dst
	}
	dst = append(dst, `,"usage":`...)
	dst = append(dst, raw...)
	return dst
}

// appendChatCompletionChunkPrefix writes the common leading fields of a
// chat.completion.chunk SSE frame.
func appendChatCompletionChunkPrefix(dst []byte, id string, created int64, model, fingerprint string) []byte {
	dst = append(dst, `{"id":`...)
	dst = strconv.AppendQuote(dst, id)
	dst = append(dst, `,"object":"chat.completion.chunk","created":`...)
	dst = strconv.AppendInt(dst, created, 10)
	dst = append(dst, `,"model":`...)
	dst = strconv.AppendQuote(dst, model)
	dst = append(dst, `,"service_tier":null`...)
	dst = append(dst, `,"system_fingerprint":`...)
	dst = strconv.AppendQuote(dst, fingerprint)
	return dst
}

// appendChatCompletionChunkFinish writes the trailing logprobs/finish/usage
// fields shared by every chat.completion.chunk frame.
func appendChatCompletionChunkFinish(dst []byte, finish string, hasFinish bool, usage map[string]interface{}) []byte {
	dst = append(dst, `,"logprobs":null,"finish_reason":`...)
	if hasFinish {
		dst = strconv.AppendQuote(dst, finish)
	} else {
		dst = append(dst, `null`...)
	}
	dst = append(dst, `}]`...)
	if hasFinish {
		dst = appendUsage(dst, usage)
	}
	dst = append(dst, '}')
	return dst
}

func appendChatCompletionChunkWithUsage(dst []byte, id string, created int64, model, fingerprint, role, content string, finish string, hasFinish bool, usage map[string]interface{}) []byte {
	dst = appendChatCompletionChunkPrefix(dst, id, created, model, fingerprint)
	dst = append(dst, `,"choices":[{"index":0,"delta":`...)
	switch {
	case role != "":
		dst = append(dst, `{"role":`...)
		dst = strconv.AppendQuote(dst, role)
		dst = append(dst, `,"content":""}`...)
	case content != "":
		dst = append(dst, `{"content":`...)
		dst = strconv.AppendQuote(dst, content)
		dst = append(dst, '}')
	default:
		dst = append(dst, `{}`...)
	}
	return appendChatCompletionChunkFinish(dst, finish, hasFinish, usage)
}

func appendChatCompletionReasoningChunk(dst []byte, id string, created int64, model, fingerprint, reasoning string) []byte {
	dst = appendChatCompletionChunkPrefix(dst, id, created, model, fingerprint)
	dst = append(dst, `,"choices":[{"index":0,"delta":{"reasoning_content":`...)
	dst = strconv.AppendQuote(dst, reasoning)
	dst = append(dst, '}')
	return appendChatCompletionChunkFinish(dst, "", false, nil)
}

func appendChatCompletionSnapshotChunkWithUsage(dst []byte, id string, created int64, model, fingerprint, messageContent string, finish string, hasFinish bool, usage map[string]interface{}) []byte {
	dst = appendChatCompletionChunkPrefix(dst, id, created, model, fingerprint)
	dst = append(dst, `,"choices":[{"index":0,"delta":{},"message":{"role":"assistant","content":`...)
	dst = strconv.AppendQuote(dst, messageContent)
	dst = append(dst, '}')
	return appendChatCompletionChunkFinish(dst, finish, hasFinish, usage)
}

func appendChatCompletionToolCallsChunkWithUsage(dst []byte, id string, created int64, model, fingerprint string, toolCalls []map[string]interface{}, finish string, hasFinish bool, usage map[string]interface{}) []byte {
	dst = appendChatCompletionChunkPrefix(dst, id, created, model, fingerprint)
	dst = append(dst, `,"choices":[{"index":0,"delta":{"tool_calls":`...)
	if len(toolCalls) == 0 {
		dst = append(dst, `[]`...)
	} else if raw, err := json.Marshal(toolCalls); err == nil {
		dst = append(dst, raw...)
	} else {
		dst = append(dst, `[]`...)
	}
	dst = append(dst, '}')
	return appendChatCompletionChunkFinish(dst, finish, hasFinish, usage)
}

// baseHostPattern matches a bare hostname or hostname:port. Anything else
// (path, query, userinfo, whitespace, control characters) is rejected so a
// forwarding header cannot smuggle extra URL structure into a generated link.
var baseHostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.\-]{0,252}[A-Za-z0-9])?(:[0-9]{1,5})?$`)

func sanitizeBaseHost(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > 260 {
		return ""
	}
	if strings.HasPrefix(value, "[") {
		host, port, err := net.SplitHostPort(value)
		if err != nil || net.ParseIP(host) == nil {
			return ""
		}
		if _, err := strconv.Atoi(port); err != nil {
			return ""
		}
		return value
	}
	if !baseHostPattern.MatchString(value) {
		return ""
	}
	return value
}

func detectPublicBaseURL(r *http.Request) string {
	proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
	if proto != "http" && proto != "https" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	// The trusted-proxy middleware strips X-Forwarded-* from untrusted peers;
	// sanitizeBaseHost keeps a surviving value from carrying URL structure.
	host := sanitizeBaseHost(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = sanitizeBaseHost(r.Host)
	}
	if host == "" {
		return ""
	}
	return proto + "://" + host
}

func isThinkingResponse(resp map[string]interface{}) bool {
	if resp == nil {
		return false
	}
	for _, path := range [][]string{
		{"isThinking"}, {"is_thinking"},
		{"response", "isThinking"}, {"response", "is_thinking"},
		{"result", "isThinking"}, {"result", "is_thinking"},
		{"result", "response", "isThinking"}, {"result", "response", "is_thinking"},
	} {
		raw := valueAtPath(resp, path...)
		if raw == nil {
			continue
		}
		val, err := parseLooseBoolAnyForField(raw, path[len(path)-1])
		if err == nil {
			return val
		}
	}
	return false
}

func (h *Handler) defaultChatStream() bool {
	if h == nil || h.configSnapshot() == nil {
		return true
	}
	return config.ChatDefaultStream
}

func (h *Handler) applyDefaultChatStream(req *ChatCompletionsRequest) {
	if req == nil {
		return
	}
	if req.StreamProvided {
		return
	}
	req.Stream = h.defaultChatStream()
}

func (h *Handler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req ChatCompletionsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	req.startedAt = time.Now()
	req.sourceOperation, _ = r.Context().Value(chatSourceOperationKey{}).(string)
	verboseDiagnostics := logutil.VerboseDiagnosticsEnabled()
	debugLogSSE := h != nil && h.configSnapshot() != nil && h.configSnapshot().DebugLogSSE
	logger := debug.NewForContext(r.Context(), verboseDiagnostics, verboseDiagnostics && debugLogSSE)
	defer logger.Close()
	logger.LogIncomingRequest(req)
	req.Model = normalizeModelID(req.Model)
	// Publish the resolved model so the per-minute aggregation can attribute this
	// request to a model: the latency middleware cannot read the body itself.
	r = r.WithContext(middleware.WithRequestModel(r.Context(), req.Model))
	if req.ImageConfig != nil {
		req.ImageConfig.Normalize()
	}
	h.applyDefaultChatStream(&req)
	if err := req.Validate(); err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	if req.ImageConfig != nil && !isImageGenerationModel(req.Model) && !isImageEditModel(req.Model) {
		originalModel := req.Model
		req.Model = "grok-imagine-image"
		slog.Info("Auto mapped image_config request to image model", "from", originalModel, "to", req.Model)
	}
	if !requireAPIKeyModel(w, r, req.Model) {
		return
	}
	session := sessionFromContext(r.Context())
	if session.Key == "" {
		session = prepareGrokSession(r, req.Model, req.PromptCacheKey, req.Messages)
	}
	if session.Key != "" {
		req.PromptCacheKey = session.Key
		req.ReasoningReplay = session.Replay
		r = r.WithContext(withGrokSession(r.Context(), session))
	}
	spec, ok := h.resolveConversationModel(r.Context(), req.Model)
	if !ok {
		writeGrokErrorCode(w, http.StatusNotFound, "model_not_found", modelNotFoundMessage(req.Model))
		return
	}
	if spec.AliasReasoningEffort != "" {
		effort := spec.AliasReasoningEffort
		req.ReasoningEffort = &effort
	}
	if err := h.ensureResolvedModelCapability(r.Context(), spec.ID, spec, store.CapabilityChat); err != nil {
		if strings.EqualFold(strings.TrimSpace(err.Error()), "model not found") {
			writeGrokErrorCode(w, http.StatusNotFound, "model_not_found", modelNotFoundMessage(req.Model))
			return
		}
		writeGrokError(w, http.StatusBadRequest, modelValidationMessage(req.Model, err))
		return
	}
	if !spec.SupportsConversation() {
		writeGrokError(w, http.StatusBadRequest, fmt.Sprintf("model %s does not support chat completions", req.Model))
		return
	}

	publicBase := detectPublicBaseURL(r)
	if spec.IsImage {
		prompt, imageURLs := extractPromptAndImageURLs(req.Messages)
		prompt = strings.TrimSpace(prompt)
		if prompt == "" {
			writeGrokError(w, http.StatusBadRequest, "prompt is required")
			return
		}
		imageCfg := req.ImageConfig
		if imageCfg == nil {
			imageCfg = &ImageConfig{}
			imageCfg.Normalize()
		}
		if imageCfg.N < 1 || imageCfg.N > 10 {
			writeGrokError(w, http.StatusBadRequest, "image_config.n must be between 1 and 10")
			return
		}
		// The same transport-layer rules the image endpoints apply: a ratio is a
		// ratio, and a resolution tier is 1k or 2k.
		if edgeAspect := strings.TrimSpace(imageCfg.AspectRatio); edgeAspect != "" && !validImageAspectRatio(edgeAspect) {
			writeGrokErrorCode(w, http.StatusBadRequest, "invalid_parameter", "aspect_ratio is not supported")
			return
		}
		if _, err := normalizeImageResolution(imageCfg.Resolution); err != nil {
			writeGrokErrorCode(w, http.StatusBadRequest, "invalid_parameter", err.Error())
			return
		}
		if isImageGenerationModel(req.Model) && normalizeModelID(req.Model) == "grok-imagine-image-lite" && imageCfg.N > 4 {
			writeGrokError(w, http.StatusBadRequest, "image_config.n must be between 1 and 4 for grok-imagine-image-lite")
			return
		}
		if isImageEditModel(req.Model) && imageCfg.N > 2 {
			writeGrokError(w, http.StatusBadRequest, "image_config.n must be between 1 and 2 for image edit")
			return
		}
		if req.Stream && imageCfg.N > 2 {
			writeGrokError(w, http.StatusBadRequest, "streaming is only supported when image_config.n=1 or n=2")
			return
		}
		imageCfg.ResponseFormat = normalizeImageResponseFormat(imageCfg.ResponseFormat)
		if imageCfg.Size != "" {
			var (
				size string
				err  error
			)
			if isImageEditModel(req.Model) {
				size, err = normalizeImageEditSize(imageCfg.Size)
			} else {
				size, err = normalizeImageSize(imageCfg.Size)
			}
			if err != nil {
				writeGrokUpstreamError(w, err)
				return
			}
			imageCfg.Size = size
		} else {
			imageCfg.Size = "1024x1024"
		}

		if isImageEditModel(spec.ID) {
			if len(imageURLs) == 0 {
				writeGrokError(w, http.StatusBadRequest, "image_url is required for image edits")
				return
			}
			editInputs := imageURLs
			if len(editInputs) > 7 {
				editInputs = editInputs[len(editInputs)-7:]
			}
			h.handleChatImageEdit(r.Context(), w, req, spec, prompt, editInputs, publicBase)
			return
		}

		// image_config carries the ratio and the resolution tier in grok2api, so a
		// chat image request can ask for either instead of only reaching the
		// defaults the top-level fields would have produced.
		genReq := ImagesGenerationsRequest{
			Model:          req.Model,
			Prompt:         prompt,
			N:              imageCfg.N,
			Size:           imageCfg.Size,
			AspectRatio:    strings.TrimSpace(imageCfg.AspectRatio),
			Resolution:     strings.TrimSpace(imageCfg.Resolution),
			Stream:         req.Stream,
			ResponseFormat: imageCfg.ResponseFormat,
		}
		h.serveImagesGenerations(r.Context(), w, genReq, publicBase)
		return
	}

	if err := validateWebToolDefinitions(req.Tools); err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	parallelToolCalls := true
	if req.ParallelToolCalls != nil {
		parallelToolCalls = *req.ParallelToolCalls
	}
	text, attachments, err := extractMessageAndAttachmentsWithTools(req.Messages, spec.IsVideo, req.Tools, req.ToolChoice, parallelToolCalls)
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	// One request used to be able to name unbounded attachments, each fetched
	// and base64-expanded server-side. grok2api caps the count and the combined
	// size; the same ceiling keeps a single caller from monopolising the egress
	// and the memory budget.
	if len(attachments) > maxChatAttachments {
		writeGrokError(w, http.StatusBadRequest,
			fmt.Sprintf("at most %d attachments are supported per request", maxChatAttachments))
		return
	}
	userPrompt := strings.TrimSpace(extractLastUserText(req.Messages))
	if userPrompt == "" {
		userPrompt = strings.TrimSpace(text)
	}
	slog.Debug("grok chat prompt context",
		"model", req.Model,
		"history_chars", utf8.RuneCountInString(strings.TrimSpace(text)),
		"latest_user_chars", utf8.RuneCountInString(userPrompt),
		"attachments", len(attachments),
		"stream", req.Stream,
	)
	if strings.TrimSpace(text) == "" && len(attachments) == 0 {
		writeGrokError(w, http.StatusBadRequest, "empty message")
		return
	}

	if spec.IsVideo {
		if cfg, err := validateVideoConfig(req.VideoConfig); err != nil {
			writeGrokUpstreamError(w, err)
			return
		} else {
			req.VideoConfig = cfg
		}
		videoPrompt, videoAttachments, err := extractVideoPromptAndAttachments(req.Messages)
		if err != nil {
			writeGrokUpstreamError(w, err)
			return
		}
		h.serveVideoChatCompletion(r.Context(), w, &req, spec, videoPrompt, videoAttachments, publicBase, logger)
		return
	}

	// The request's model travels on the context so account selection can skip an
	// account that is cooling down for this model only; the account's other models
	// stay in the pool.
	r = r.WithContext(WithRequestModel(r.Context(), spec.ID))

	sess, err := h.openChatAccountSessionForModel(r.Context(), spec)
	if err != nil {
		writeGrokNoAccountError(w, err)
		return
	}
	defer sess.Close()
	req.account = sess.acc

	buildPayload := func(token string) (map[string]interface{}, error) {
		fileAttachments := []string(nil)
		if !spec.IsVideo {
			var upErr error
			fileAttachments, upErr = h.uploadAttachmentInputs(r.Context(), token, attachments)
			if upErr != nil {
				return nil, fmt.Errorf("attachment upload failed: %w", upErr)
			}
		}
		return h.buildChatPayload(r.Context(), token, spec, text, fileAttachments, attachments, req.VideoConfig, &req)
	}

	payload, err := buildPayload(sess.token)
	if err != nil {
		if skipExternalAttachmentFetchGrokAccountStatus(err) {
			h.markAccountStatus(r.Context(), sess.acc, err)
		}
		writeGrokUpstreamError(w, err)
		return
	}
	if !logger.Capturing() {
		logger.LogUpstreamRequest(h.webClient().baseURL()+defaultChatPath, debugHeaderMap(h.webClient().appChatHeaders(sess.token)), payload)
	}

	resp, err := h.doChatWithAutoSwitchRebuild(r.Context(), sess, &payload, buildPayload)
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	h.syncGrokQuota(sess.acc, resp.Header)

	hasAttachments := len(attachments) > 0
	if req.Stream {
		h.streamChat(w, &req, req.Model, spec, sess.token, publicBase, hasAttachments, req.Tools, req.ToolChoice, resp.Body, logger)
		return
	}
	h.collectChat(w, &req, req.Model, spec, sess.token, publicBase, hasAttachments, req.Tools, req.ToolChoice, resp.Body, logger)
}

func (h *Handler) buildChatPayload(
	ctx context.Context,
	token string,
	spec ModelSpec,
	text string,
	fileAttachments []string,
	attachmentInputs []AttachmentInput,
	videoCfg *VideoConfig,
	req *ChatCompletionsRequest,
) (map[string]interface{}, error) {
	payload := h.webClient().chatPayload(spec, text, true, 0)
	if len(fileAttachments) > 0 {
		payload["fileAttachments"] = fileAttachments
	}
	if req != nil {
		override := map[string]interface{}{}
		if req.Temperature != nil {
			override["temperature"] = *req.Temperature
		}
		if req.TopP != nil {
			override["topP"] = *req.TopP
		}
		if req.ReasoningEffort != nil {
			if v := strings.TrimSpace(*req.ReasoningEffort); v != "" {
				override["reasoningEffort"] = strings.ToLower(v)
			}
		}
		if len(override) > 0 {
			responseMetadata, _ := payload["responseMetadata"].(map[string]interface{})
			if responseMetadata == nil {
				responseMetadata = map[string]interface{}{}
				payload["responseMetadata"] = responseMetadata
			}
			modelConfigOverride, _ := responseMetadata["modelConfigOverride"].(map[string]interface{})
			if modelConfigOverride == nil {
				modelConfigOverride = map[string]interface{}{}
				responseMetadata["modelConfigOverride"] = modelConfigOverride
			}
			for k, v := range override {
				modelConfigOverride[k] = v
			}
		}
	}

	if !spec.IsVideo {
		return payload, nil
	}
	return h.buildVideoCreatePayload(ctx, token, spec, text, attachmentInputs, videoCfg)
}

func (h *Handler) buildVideoCreatePayload(
	ctx context.Context,
	token string,
	spec ModelSpec,
	text string,
	attachmentInputs []AttachmentInput,
	videoCfg *VideoConfig,
) (map[string]interface{}, error) {
	_ = ctx
	_ = token
	if videoCfg == nil {
		videoCfg = &VideoConfig{}
	}
	videoCfg.Normalize()
	for _, item := range attachmentInputs {
		if strings.EqualFold(strings.TrimSpace(item.Type), "image") && strings.TrimSpace(item.Data) != "" {
			return nil, fmt.Errorf("web video generation currently supports text-to-video only; use a Console/Build video model for image references")
		}
	}
	prompt := strings.TrimSpace(text)
	payload := map[string]interface{}{
		"modelName":            firstNonEmpty(strings.TrimSpace(spec.UpstreamModel), "imagine-video-gen"),
		"message":              prompt + " --mode=custom",
		"enableImageStreaming": true,
		"enableSideBySide":     true,
		"sendFinalMetadata":    true,
		"responseMetadata": map[string]interface{}{
			"experiments": []interface{}{},
			"modelConfigOverride": map[string]interface{}{
				"modelMap": map[string]interface{}{},
			},
		},
		"mediaGenInput": map[string]interface{}{
			"textToVideo": map[string]interface{}{
				"prompt":         prompt,
				"aspectRatio":    videoCfg.AspectRatio,
				"duration":       videoCfg.VideoLength,
				"resolutionName": videoCfg.ResolutionName,
			},
		},
		"kind": "CONVERSATION_KIND_IMAGINE",
	}
	return payload, nil
}

func (h *Handler) buildVideoExtendPayload(spec ModelSpec, prompt string, parentPostID string, extendPostID string, videoCfg *VideoConfig, segmentLength int, elapsedSeconds int) map[string]interface{} {
	if videoCfg == nil {
		videoCfg = &VideoConfig{}
	}
	videoCfg.Normalize()
	message := strings.TrimSpace(prompt)
	if modeFlag := videoPresetFlag(videoCfg.Preset); modeFlag != "" {
		message = strings.TrimSpace(message + " " + modeFlag)
	}
	temporary := true
	if h != nil && h.configSnapshot() != nil {
		temporary = h.configSnapshot().GrokChatTemporary()
	}
	payload := map[string]interface{}{
		"temporary":        temporary,
		"modelName":        spec.UpstreamModel,
		"message":          message,
		"enableSideBySide": true,
		"responseMetadata": map[string]interface{}{
			"experiments": []interface{}{},
			"modelConfigOverride": map[string]interface{}{
				"modelMap": map[string]interface{}{
					"videoGenModelConfig": map[string]interface{}{
						"isVideoExtension":        true,
						"videoExtensionStartTime": videoExtensionStartTime(elapsedSeconds),
						"extendPostId":            strings.TrimSpace(extendPostID),
						"stitchWithExtendPostId":  true,
						"originalPrompt":          strings.TrimSpace(prompt),
						"originalPostId":          strings.TrimSpace(parentPostID),
						"originalRefType":         "ORIGINAL_REF_TYPE_VIDEO_EXTENSION",
						"mode":                    strings.TrimSpace(videoCfg.Preset),
						"aspectRatio":             videoCfg.AspectRatio,
						"videoLength":             segmentLength,
						"resolutionName":          videoCfg.ResolutionName,
						"parentPostId":            strings.TrimSpace(parentPostID),
						"isVideoEdit":             false,
					},
				},
			},
			"requestModelDetails": map[string]interface{}{
				"modelId": spec.UpstreamModel,
			},
		},
	}
	if strings.TrimSpace(spec.ModelMode) != "" {
		payload["modelMode"] = spec.ModelMode
	}
	return payload
}

func videoPresetFlag(preset string) string {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "fun":
		return "--mode=extremely-crazy"
	case "normal":
		return "--mode=normal"
	case "spicy":
		return "--mode=extremely-spicy-or-crazy"
	case "", "custom":
		return "--mode=custom"
	default:
		return "--mode=custom"
	}
}

func (h *Handler) uploadAttachmentInputs(ctx context.Context, token string, inputs []AttachmentInput) ([]string, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(inputs))
	for _, item := range inputs {
		fileID, fileURI, err := h.uploadSingleInput(ctx, token, item.Data)
		if err != nil {
			return nil, err
		}
		id := strings.TrimSpace(fileID)
		if id == "" {
			id = strings.TrimSpace(fileURI)
		}
		if id != "" {
			out = append(out, id)
		}
	}
	return uniqueStrings(out), nil
}

func (h *Handler) uploadSingleInput(ctx context.Context, token, input string) (string, string, error) {
	data := strings.TrimSpace(input)
	if data == "" {
		return "", "", fmt.Errorf("empty attachment")
	}
	if isRemoteURL(data) {
		var err error
		proxyFunc := http.ProxyFromEnvironment
		if h != nil && h.configSnapshot() != nil {
			proxyFunc = httpclient.ProxyFuncFromConfig(h.configSnapshot())
		}
		data, err = fetchRemoteAsDataURI(data, 30*time.Second, proxyFunc)
		if err != nil {
			return "", "", err
		}
	}
	filename, contentBase64, mime, err := parseDataURI(data)
	if err != nil {
		// Fallback for plain base64 payloads.
		filename = "file.bin"
		mime = "application/octet-stream"
		contentBase64 = data
	}
	return h.webClient().uploadFile(ctx, token, filename, mime, contentBase64)
}

type streamMarkupFilter struct {
	pending  string
	inTool   bool
	inRender bool
}

const (
	streamToolStartTag   = "xai:tool_usage_card"
	streamToolEndTag     = "</xai:tool_usage_card>"
	streamRenderStartTag = "<grok:render"
	streamRenderEndTag   = "</grok:render>"
)

func asciiLowerByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if asciiLowerByte(a[i]) != asciiLowerByte(b[i]) {
			return false
		}
	}
	return true
}

func indexFoldASCII(s, sub string) int {
	if sub == "" {
		return 0
	}
	n := len(sub)
	if len(s) < n {
		return -1
	}
	first := asciiLowerByte(sub[0])
	for i := 0; i <= len(s)-n; i++ {
		if asciiLowerByte(s[i]) != first {
			continue
		}
		if equalFoldASCII(s[i:i+n], sub) {
			return i
		}
	}
	return -1
}

// Streaming sanitizer/tokenizer.
// Goal: never leak tool/render markup, never corrupt UTF-8, and keep streaming responsive.
func (f *streamMarkupFilter) feed(chunk string) string {
	if f == nil || chunk == "" {
		return ""
	}
	f.pending += chunk
	// Bound memory
	if len(f.pending) > 64*1024 {
		f.pending = f.pending[len(f.pending)-64*1024:]
	}

	var out strings.Builder
	endsWithNewline := false
	writeOut := func(s string) {
		if s == "" {
			return
		}
		out.WriteString(s)
		endsWithNewline = s[len(s)-1] == '\n'
	}

	for {
		if f.inTool {
			end := indexFoldASCII(f.pending, streamToolEndTag)
			if end < 0 {
				// wait for more data
				break
			}
			raw := f.pending[:end+len(streamToolEndTag)]
			if line := extractToolUsageCardText(raw); line != "" {
				if out.Len() > 0 && !endsWithNewline {
					writeOut("\n")
				}
				writeOut(line)
				writeOut("\n")
			}
			f.pending = f.pending[end+len(streamToolEndTag):]
			f.inTool = false
			continue
		}
		if f.inRender {
			end := indexFoldASCII(f.pending, streamRenderEndTag)
			if end < 0 {
				break
			}
			f.pending = f.pending[end+len(streamRenderEndTag):]
			f.inRender = false
			continue
		}

		idxTool := indexFoldASCII(f.pending, streamToolStartTag)
		idxRender := indexFoldASCII(f.pending, streamRenderStartTag)
		idx := -1
		kind := ""
		if idxTool >= 0 {
			idx = idxTool
			kind = "tool"
		}
		if idxRender >= 0 && (idx < 0 || idxRender < idx) {
			idx = idxRender
			kind = "render"
		}

		if idx < 0 {
			// No markers. Emit everything except a tail to avoid cutting potential markers.
			keep := 512
			if len(f.pending) <= keep {
				break
			}
			safe := validUTF8Prefix(f.pending[:len(f.pending)-keep])
			safe = sanitizeText(safe)
			if safe != "" {
				writeOut(safe)
			}
			f.pending = f.pending[len(f.pending)-keep:]
			break
		}

		// Emit prefix before marker
		prefix := validUTF8Prefix(f.pending[:idx])
		prefix = sanitizeText(prefix)
		if prefix != "" {
			writeOut(prefix)
		}
		f.pending = f.pending[idx:]
		if kind == "tool" {
			f.inTool = true
		} else {
			f.inRender = true
		}
	}

	return out.String()
}

func (f *streamMarkupFilter) flush() string {
	if f == nil {
		return ""
	}
	if f.inTool || f.inRender {
		return ""
	}
	if strings.TrimSpace(f.pending) == "" {
		return ""
	}
	out := sanitizeText(stripToolAndRenderMarkup(validUTF8Prefix(f.pending)))
	f.pending = ""
	return out
}

func validUTF8Prefix(s string) string {
	if s == "" || utf8.ValidString(s) {
		return s
	}
	// Trim bytes until valid UTF-8.
	b := []byte(s)
	for len(b) > 0 {
		b = b[:len(b)-1]
		if utf8.Valid(b) {
			return string(b)
		}
	}
	return ""
}

func sanitizeUpstreamText(raw string) string {
	return sanitizeText(stripToolAndRenderMarkup(raw))
}

const streamImageRefTailKeep = 1024

type streamTextImageRefCollector struct {
	tail      string
	urls      []string
	assets    []string
	seenURLs  map[string]struct{}
	seenAsset map[string]struct{}
}

func newStreamTextImageRefCollector() *streamTextImageRefCollector {
	return &streamTextImageRefCollector{
		urls:      make([]string, 0, 8),
		assets:    make([]string, 0, 8),
		seenURLs:  map[string]struct{}{},
		seenAsset: map[string]struct{}{},
	}
}

func streamRefMaybePresent(s string) bool {
	if s == "" {
		return false
	}
	return strings.Contains(s, "http://") ||
		strings.Contains(s, "https://") ||
		strings.Contains(s, "assets.grok.com") ||
		strings.Contains(s, "/generated/") ||
		strings.Contains(s, "users/") ||
		strings.Contains(s, "user/") ||
		strings.Contains(s, ".png") ||
		strings.Contains(s, ".jpg") ||
		strings.Contains(s, ".jpeg") ||
		strings.Contains(s, ".webp") ||
		strings.Contains(s, ".gif")
}

func keepLastBytes(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	return s[len(s)-maxLen:]
}

func (c *streamTextImageRefCollector) feed(raw string) {
	if c == nil || strings.TrimSpace(raw) == "" {
		return
	}
	scan := raw
	if c.tail != "" {
		scan = c.tail + raw
	}
	if streamRefMaybePresent(scan) {
		for _, u := range extractImageURLsFromText(scan) {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			if _, ok := c.seenURLs[u]; ok {
				continue
			}
			c.seenURLs[u] = struct{}{}
			c.urls = append(c.urls, u)
		}
		for _, p := range extractGrokAssetPathsFromText(scan) {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, ok := c.seenAsset[p]; ok {
				continue
			}
			c.seenAsset[p] = struct{}{}
			c.assets = append(c.assets, p)
		}
	}
	c.tail = keepLastBytes(scan, streamImageRefTailKeep)
}

func (c *streamTextImageRefCollector) emit(emitURL func(string)) {
	if c == nil || emitURL == nil {
		return
	}
	for _, u := range c.urls {
		emitURL(u)
	}
	for _, p := range c.assets {
		emitURL("https://assets.grok.com/" + strings.TrimPrefix(p, "/"))
	}
}

func forEachImageCandidateFromValue(value interface{}, includeStructured bool, includeAssetLike bool, assetLimit int, emitURL func(string)) {
	if emitURL == nil {
		return
	}
	if includeStructured {
		for _, u := range extractImageURLs(value) {
			emitURL(u)
		}
	}
	for _, u := range extractRenderableImageLinks(value) {
		emitURL(u)
	}
	if !includeAssetLike {
		return
	}
	for _, p := range collectAssetLikeStrings(value, assetLimit) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if isLikelyImageURL(p) {
			emitURL(p)
			continue
		}
		if isLikelyImageAssetPath(p) {
			emitURL("https://assets.grok.com/" + strings.TrimPrefix(p, "/"))
		}
	}
}

// NOTE: streamMarkupFilter.feed is implemented earlier in this file.

func debugHeaderMap(headers http.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for k, values := range headers {
		if isSensitiveUpstreamHeader(k) {
			out[k] = "[redacted]"
			continue
		}
		out[k] = strings.Join(values, ", ")
	}
	return out
}

func streamMessageDelta(previous, current string) string {
	if current == "" || current == previous {
		return ""
	}
	if previous == "" {
		return current
	}
	if strings.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	prevRunes := []rune(previous)
	currRunes := []rune(current)
	maxPrefix := len(prevRunes)
	if len(currRunes) < maxPrefix {
		maxPrefix = len(currRunes)
	}
	prefix := 0
	for prefix < maxPrefix && prevRunes[prefix] == currRunes[prefix] {
		prefix++
	}
	if prefix >= len(currRunes) {
		return ""
	}
	return string(currRunes[prefix:])
}

func commonPrefixText(a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	ar := []rune(a)
	br := []rune(b)
	limit := len(ar)
	if len(br) < limit {
		limit = len(br)
	}
	idx := 0
	for idx < limit && ar[idx] == br[idx] {
		idx++
	}
	if idx == 0 {
		return ""
	}
	return string(ar[:idx])
}

func toolCallsEnabled(tools []ToolDef, toolChoice interface{}) bool {
	if len(tools) == 0 {
		return false
	}
	if s, ok := toolChoice.(string); ok && strings.EqualFold(strings.TrimSpace(s), "none") {
		return false
	}
	return true
}

func suffixPrefixOverlap(text, tag string) int {
	if text == "" || tag == "" {
		return 0
	}
	maxKeep := len(text)
	if limit := len(tag) - 1; maxKeep > limit {
		maxKeep = limit
	}
	for keep := maxKeep; keep > 0; keep-- {
		if strings.HasSuffix(text, tag[:keep]) {
			return keep
		}
	}
	return 0
}

func (h *Handler) streamChat(w http.ResponseWriter, req *ChatCompletionsRequest, model string, spec ModelSpec, token string, publicBase string, hasAttachments bool, tools []ToolDef, toolChoice interface{}, body io.Reader, logger *debug.Logger) {
	flusher := streamResponseHeaders(w)

	id := "chatcmpl_" + randomHex(8)
	fingerprint := ""
	sentRole := false
	lastMessage := ""
	sentAny := false
	var finalUsage map[string]interface{}
	defer func() {
		if len(finalUsage) > 0 {
			h.auditChatOutcome(context.Background(), req.account, req, chatOutcome{Finish: "stop", Usage: finalUsage, UsageSource: audit.UsageSourceEstimated})
		}
	}()
	enableMediaExtraction := hasAttachments || spec.IsVideo
	// Image URL stream handling: prefer full image variants over -part-0 previews.
	seenFull := map[string]bool(nil)
	pendingPart := map[string]string(nil)
	emitted := map[string]bool(nil)
	if enableMediaExtraction {
		seenFull = map[string]bool{}
		pendingPart = map[string]string{}
		emitted = map[string]bool{}
	}
	sawModelMessage := false
	var tokenFallback strings.Builder
	var reasoningContent strings.Builder
	lastThinkingMessage := ""
	emittedModelMessage := ""
	pendingModelMessage := ""
	toolStreamMode := toolCallsEnabled(tools, toolChoice)
	toolParser := newToolCallParser(tools, toolChoice)
	finalSnapshotEnabled := !toolStreamMode && !spec.IsVideo
	toolCallIndex := 0
	var toolPump *toolStreamPump
	if toolStreamMode {
		toolPump = newToolStreamPump(toolParser)
	}
	finalSnapshotMarkdown := make([]string, 0, 4)

	var mf *streamMarkupFilter
	if !hasAttachments && !toolStreamMode {
		mf = &streamMarkupFilter{}
	}
	var textRefCollector *streamTextImageRefCollector
	if enableMediaExtraction {
		textRefCollector = newStreamTextImageRefCollector()
	}
	chunkScratch := make([]byte, 0, 256)
	var downstreamWriteErr error
	emitRaw := func(raw []byte) {
		if downstreamWriteErr == nil {
			downstreamWriteErr = writeSSELog(w, flusher, logger, raw)
		}
	}

	emitChunk := func(role, content string, finish string, hasFinish bool) {
		raw := appendChatCompletionChunkWithUsage(chunkScratch[:0], id, time.Now().Unix(), model, fingerprint, role, content, finish, hasFinish, finalUsage)
		chunkScratch = raw[:0]
		emitRaw(raw)
		sentAny = true
	}

	emitTextChunk := func(content string) {
		if toolStreamMode {
			return
		}
		if content != "" {
			emitChunk("", content, "", false)
		}
	}

	emitReasoningChunk := func(content string) {
		if content == "" {
			return
		}
		raw := appendChatCompletionReasoningChunk(chunkScratch[:0], id, time.Now().Unix(), model, fingerprint, content)
		chunkScratch = raw[:0]
		emitRaw(raw)
		sentAny = true
	}

	emitCleanText := func(raw string) {
		if raw == "" {
			return
		}
		if toolStreamMode {
			return
		}
		if mf == nil {
			if cleaned := sanitizeUpstreamText(raw); cleaned != "" {
				emitTextChunk(cleaned)
			}
			return
		}
		if cleaned := mf.feed(raw); cleaned != "" {
			emitTextChunk(cleaned)
		}
	}

	emitImageURL := func(raw string) {
		if !enableMediaExtraction {
			return
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		// Track preview vs full variants on the raw upstream URL.
		if strings.Contains(raw, "-part-0/") {
			full := strings.ReplaceAll(raw, "-part-0/", "/")
			if seenFull[full] {
				return
			}
			pendingPart[full] = raw
			return
		}
		seenFull[raw] = true
		if emitted[raw] {
			return
		}

		val, errV := h.imageOutputValue(context.Background(), token, raw, "url")
		if errV != nil || strings.TrimSpace(val) == "" {
			val = raw
		}
		if publicBase != "" && strings.HasPrefix(val, "/") {
			val = publicBase + val
		}
		if md := formatImageMarkdown(val); md != "" {
			finalSnapshotMarkdown = append(finalSnapshotMarkdown, md)
			emitChunk("", md, "", false)
			emitted[raw] = true
		}
	}

	emitToolCallsChunk := func(content string, toolCalls []map[string]interface{}, finish string, hasFinish bool) {
		if strings.TrimSpace(content) != "" {
			emitChunk("", strings.TrimSpace(content), "", false)
		}
		if len(toolCalls) == 0 {
			return
		}
		indexedToolCalls := make([]map[string]interface{}, 0, len(toolCalls))
		for _, tc := range toolCalls {
			if tc == nil {
				continue
			}
			cp := make(map[string]interface{}, len(tc)+1)
			for k, v := range tc {
				cp[k] = v
			}
			if _, ok := cp["index"]; !ok {
				cp["index"] = toolCallIndex
			}
			toolCallIndex++
			indexedToolCalls = append(indexedToolCalls, cp)
		}
		if len(indexedToolCalls) == 0 {
			return
		}
		raw := appendChatCompletionToolCallsChunkWithUsage(chunkScratch[:0], id, time.Now().Unix(), model, fingerprint, indexedToolCalls, finish, hasFinish, finalUsage)
		chunkScratch = raw[:0]
		emitRaw(raw)
		sentAny = true
	}

	onToolStreamText := func(s string) {
		emitChunk("", s, "", false)
	}
	onToolStreamCall := func(toolCall map[string]interface{}) {
		emitToolCallsChunk("", []map[string]interface{}{toolCall}, "", false)
	}
	handleToolStreamChunk := func(chunk string) {
		if toolPump != nil {
			toolPump.feed(chunk, onToolStreamText, onToolStreamCall)
		}
	}
	flushToolStreamChunk := func() {
		if toolPump != nil {
			toolPump.flush(onToolStreamText, onToolStreamCall)
		}
	}

	err := parseUpstreamLines(body, func(resp map[string]interface{}) error {
		if downstreamWriteErr != nil {
			return downstreamWriteErr
		}
		if logger != nil {
			if raw, err := json.Marshal(resp); err == nil {
				logger.LogOutputSSE("response", string(raw))
			}
		}
		mr := extractUpstreamModelResponse(resp)
		if fingerprint == "" {
			fingerprint = extractUpstreamFingerprint(resp, mr)
		}
		if rid := extractUpstreamResponseID(resp, mr); rid != "" {
			id = rid
		}
		if !sentRole {
			emitChunk("assistant", "", "", false)
			sentRole = true
		}
		tokenDelta := extractUpstreamTokenDelta(resp, mr)
		if isThinkingResponse(resp) {
			thinkingDelta := tokenDelta
			if thinkingDelta == "" && mr != nil {
				if snapshot := extractUpstreamMessage(mr); snapshot != "" && snapshot != lastThinkingMessage {
					thinkingDelta = streamMessageDelta(lastThinkingMessage, snapshot)
					lastThinkingMessage = snapshot
				}
			}
			if thinkingDelta != "" {
				reasoningContent.WriteString(thinkingDelta)
				emitReasoningChunk(thinkingDelta)
			}
			return nil
		}
		if tokenDelta != "" {
			if textRefCollector != nil {
				textRefCollector.feed(tokenDelta)
			}
			if toolStreamMode {
				handleToolStreamChunk(tokenDelta)
			} else if !sawModelMessage {
				tokenFallback.WriteString(tokenDelta)
			}
		}
		if mr != nil {
			if msg := extractUpstreamMessage(mr); strings.TrimSpace(msg) != "" && msg != lastMessage {
				lastMessage = msg
				sawModelMessage = true
				if textRefCollector != nil {
					textRefCollector.feed(msg)
				}
				if pendingModelMessage != "" {
					stable := commonPrefixText(pendingModelMessage, msg)
					if stable != "" && stable != emittedModelMessage && strings.HasPrefix(stable, emittedModelMessage) {
						if delta := streamMessageDelta(emittedModelMessage, stable); delta != "" {
							emitCleanText(delta)
							emittedModelMessage = stable
						}
					}
				}
				pendingModelMessage = msg
				if toolStreamMode && toolPump != nil && !toolPump.SawEvents && !toolPump.EmittedToolCalls {
					textContent, toolCalls := toolParser.parseCalls(strings.TrimSpace(msg))
					if len(toolCalls) > 0 {
						emitToolCallsChunk(textContent, toolCalls, "", false)
						if toolPump != nil {
							toolPump.EmittedToolCalls = true
						}
						emittedModelMessage = msg
					}
				}
				if strings.Contains(msg, "<grok:render") || strings.Contains(msg, "tool_usage_card") {
					slog.Debug("grok message contains render/tool markup", "has_modelResponse", true)
				}
			}
			if enableMediaExtraction {
				forEachImageCandidateFromValue(mr, true, true, 80, emitImageURL)
			}
		}
		// Broader fallback: sometimes URLs live outside modelResponse.
		if enableMediaExtraction {
			forEachImageCandidateFromValue(resp, true, true, 120, emitImageURL)
		}
		if spec.IsVideo {
			if progress, videoURL, ok := extractVideoProgress(resp); ok {
				if progress > 0 && progress < 100 {
					emitChunk("", fmt.Sprintf("正在生成视频中，当前进度%d%%\n", progress), "", false)
				}
				if progress >= 100 && strings.TrimSpace(videoURL) == "" {
					videoURL = firstAssetURL(resp)
				}
				if progress >= 100 && strings.TrimSpace(videoURL) != "" {
					finalURL := strings.TrimSpace(videoURL)
					if name, err := h.cacheMediaURL(context.Background(), token, finalURL, "video"); err == nil && name != "" {
						finalURL = "/grok/v1/files/video/" + name
					}
					if publicBase != "" && strings.HasPrefix(finalURL, "/") {
						finalURL = publicBase + finalURL
					}
					emitChunk("", finalURL, "", false)
				}
			}
		}
		return nil
	})
	if err == nil && downstreamWriteErr != nil {
		err = downstreamWriteErr
	}
	if err != nil {
		slog.Warn("grok stream parse failed", "error", err)
		if !sentAny {
			// A stalled upstream is not a parse error: the idle watchdog's sentinel
			// gets its own text and its own code so a client can tell the two apart.
			if errors.Is(err, ErrGrokSemanticIdle) {
				writeChatStreamError(w, flusher, logger, "upstream stream timed out while waiting for generated output", "upstream_stream_idle_timeout")
			} else {
				writeChatStreamError(w, flusher, logger, "stream parse error: "+err.Error(), "stream_error")
			}
			return
		}
		// A stalled upstream is not a parse error: the idle watchdog's sentinel
		// gets its own text and its own code so a client can tell the two apart.
		if errors.Is(err, ErrGrokSemanticIdle) {
			writeChatStreamError(w, flusher, logger, "upstream stream timed out while waiting for generated output", "upstream_stream_idle_timeout")
		} else {
			writeChatStreamError(w, flusher, logger, "stream parse error: "+err.Error(), "stream_error")
		}
		return
	}

	finalBufferedText := ""
	finalSnapshotContent := ""
	// Flush any remaining buffered text (avoids "no content" when stream ends quickly).
	if mf != nil {
		if pendingModelMessage != "" && pendingModelMessage != emittedModelMessage && strings.HasPrefix(pendingModelMessage, emittedModelMessage) {
			if delta := streamMessageDelta(emittedModelMessage, pendingModelMessage); delta != "" {
				emitCleanText(delta)
			}
		}
		if !sawModelMessage && tokenFallback.Len() > 0 {
			emitCleanText(tokenFallback.String())
		}
		if tail := mf.flush(); tail != "" {
			emitTextChunk(tail)
		}
		if tokenFallback.Len() > 0 && !sawModelMessage && h != nil && h.configSnapshot() != nil && h.configSnapshot().DebugEnabled {
			slog.Debug("grok stream fallback used token deltas (no modelResponse)", "model", model)
		}
	} else {
		if pendingModelMessage != "" && pendingModelMessage != emittedModelMessage && strings.HasPrefix(pendingModelMessage, emittedModelMessage) {
			if delta := streamMessageDelta(emittedModelMessage, pendingModelMessage); delta != "" {
				if cleaned := sanitizeUpstreamText(delta); cleaned != "" {
					if toolStreamMode {
						finalBufferedText += cleaned
					} else {
						emitTextChunk(cleaned)
					}
				}
			}
		} else if !sawModelMessage && tokenFallback.Len() > 0 {
			if cleaned := sanitizeUpstreamText(tokenFallback.String()); cleaned != "" {
				if toolStreamMode {
					finalBufferedText += cleaned
				} else {
					emitTextChunk(cleaned)
				}
			}
		}
	}
	finalUsageText := strings.TrimSpace(finalBufferedText)
	if !toolStreamMode {
		switch {
		case strings.TrimSpace(pendingModelMessage) != "":
			finalSnapshotContent = sanitizeUpstreamText(pendingModelMessage)
		case tokenFallback.Len() > 0:
			finalSnapshotContent = sanitizeUpstreamText(tokenFallback.String())
		}
		if len(finalSnapshotMarkdown) > 0 {
			finalSnapshotContent += strings.Join(finalSnapshotMarkdown, "")
		}
		if finalUsageText == "" {
			finalUsageText = strings.TrimSpace(finalSnapshotContent)
		}
	}
	// Emit any pending part-0 previews only if we never saw a full variant.
	// Try to fetch/emit the full variant first; if it doesn't exist, fall back to the preview.
	if enableMediaExtraction {
		for full, part := range pendingPart {
			if seenFull[full] {
				continue
			}
			// Try full (cache through this server for client reachability).
			if name, err := h.cacheMediaURL(context.Background(), token, full, "image"); err == nil && name != "" {
				val := "/grok/v1/files/image/" + name
				if publicBase != "" && strings.HasPrefix(val, "/") {
					val = publicBase + val
				}
				if md := formatImageMarkdown(val); md != "" {
					emitChunk("", md, "", false)
				}
				continue
			}
			// Fall back to preview.
			emitImageURL(part)
		}

		// Final pass: emit URL/path candidates found in incremental text collector.
		textRefCollector.emit(emitImageURL)
	}

	if toolStreamMode {
		flushToolStreamChunk()
		if toolPump != nil && toolPump.EmittedToolCalls {
			finalUsage = addReasoningUsage(buildChatUsagePayload(req, strings.TrimSpace(finalBufferedText), []map[string]interface{}{{"type": "function"}}), reasoningContent.String())
			emitChunk("", "", "tool_calls", true)
			emitRaw([]byte("[DONE]"))
			return
		}
		textContent, toolCalls := toolParser.parseCalls(strings.TrimSpace(finalBufferedText))
		textContent = strings.TrimSpace(textContent)
		if len(toolCalls) > 0 {
			finalUsage = addReasoningUsage(buildChatUsagePayload(req, textContent, toolCalls), reasoningContent.String())
			emitToolCallsChunk(textContent, toolCalls, "tool_calls", true)
			emitRaw([]byte("[DONE]"))
			return
		}
	}

	finalUsage = addReasoningUsage(buildChatUsagePayload(req, finalUsageText, nil), reasoningContent.String())
	if finalSnapshotEnabled && finalSnapshotContent != "" {
		raw := appendChatCompletionSnapshotChunkWithUsage(chunkScratch[:0], id, time.Now().Unix(), model, fingerprint, finalSnapshotContent, "stop", true, finalUsage)
		chunkScratch = raw[:0]
		emitRaw(raw)
	} else {
		emitChunk("", "", "stop", true)
	}
	emitRaw([]byte("[DONE]"))
}

func (h *Handler) collectChat(w http.ResponseWriter, req *ChatCompletionsRequest, model string, spec ModelSpec, token string, publicBase string, hasAttachments bool, tools []ToolDef, toolChoice interface{}, body io.Reader, logger *debug.Logger) {
	id := "chatcmpl_" + randomHex(8)
	lastMessage := ""
	videoURL := ""
	fingerprint := ""
	enableMediaExtraction := hasAttachments || spec.IsVideo
	imageCandidates := make([]string, 0, 8)
	imageCandidateSet := map[string]struct{}{}
	var tokenContent strings.Builder
	var reasoningTokens strings.Builder
	reasoningSnapshot := ""
	addImageCandidate := func(raw string) {
		u := strings.TrimSpace(raw)
		if u == "" {
			return
		}
		if _, ok := imageCandidateSet[u]; ok {
			return
		}
		imageCandidateSet[u] = struct{}{}
		imageCandidates = append(imageCandidates, u)
	}

	err := parseUpstreamLines(body, func(resp map[string]interface{}) error {
		if logger != nil {
			if raw, err := json.Marshal(resp); err == nil {
				logger.LogOutputSSE("response", string(raw))
			}
		}
		mr := extractUpstreamModelResponse(resp)
		if fingerprint == "" {
			fingerprint = extractUpstreamFingerprint(resp, mr)
		}
		if rid := extractUpstreamResponseID(resp, mr); rid != "" {
			id = rid
		}
		isThinking := isThinkingResponse(resp)
		tokenDelta := extractUpstreamTokenDelta(resp, mr)
		if isThinking {
			if tokenDelta != "" {
				reasoningTokens.WriteString(tokenDelta)
			}
			if mr != nil {
				if snapshot := extractUpstreamMessage(mr); strings.TrimSpace(snapshot) != "" {
					reasoningSnapshot = snapshot
				}
			}
			return nil
		}
		if tokenDelta != "" {
			tokenContent.WriteString(tokenDelta)
		}
		if mr != nil {
			if msg := extractUpstreamMessage(mr); strings.TrimSpace(msg) != "" && msg != lastMessage {
				lastMessage = msg
				if strings.Contains(msg, "<grok:render") || strings.Contains(msg, "tool_usage_card") {
					slog.Debug("grok message contains render/tool markup", "has_modelResponse", true)
				}
			}
			if enableMediaExtraction {
				forEachImageCandidateFromValue(mr, true, false, 0, addImageCandidate)
			}
		}
		if enableMediaExtraction {
			forEachImageCandidateFromValue(resp, false, false, 0, addImageCandidate)
		}
		if spec.IsVideo {
			if progress, vurl, ok := extractVideoProgress(resp); ok && progress >= 100 {
				videoURL = strings.TrimSpace(vurl)
				if videoURL == "" {
					videoURL = firstAssetURL(resp)
				}
			}
		}
		return nil
	})
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}

	tokenClean := sanitizeUpstreamText(tokenContent.String())
	modelClean := sanitizeUpstreamText(lastMessage)

	finalContent := modelClean
	if strings.TrimSpace(finalContent) == "" {
		finalContent = tokenClean
	}
	reasoningContent := sanitizeUpstreamText(reasoningSnapshot)
	if strings.TrimSpace(reasoningContent) == "" {
		reasoningContent = sanitizeUpstreamText(reasoningTokens.String())
	}

	var toolCalls []map[string]interface{}
	if toolCallsEnabled(tools, toolChoice) {
		textContent, parsedToolCalls := newToolCallParser(tools, toolChoice).parseCalls(finalContent)
		finalContent = strings.TrimSpace(textContent)
		toolCalls = parsedToolCalls
	}

	if videoURL != "" {
		if name, err := h.cacheMediaURL(context.Background(), token, videoURL, "video"); err == nil && name != "" {
			videoURL = "/grok/v1/files/video/" + name
		}
		if publicBase != "" && strings.HasPrefix(videoURL, "/") {
			videoURL = publicBase + videoURL
		}
		if strings.TrimSpace(finalContent) != "" {
			finalContent += "\n"
		}
		finalContent += videoURL
	}

	// Append any collected image links as Markdown, after text cleanup.
	if enableMediaExtraction {
		imgs := normalizeImageURLs(imageCandidates, 8)
		for _, u := range imgs {
			val, errV := h.imageOutputValue(context.Background(), token, u, "url")
			if errV != nil || strings.TrimSpace(val) == "" {
				val = u
			}
			if publicBase != "" && strings.HasPrefix(val, "/") {
				val = publicBase + val
			}
			finalContent += formatImageMarkdown(val)
		}
	}
	message := map[string]interface{}{
		"role":        "assistant",
		"content":     finalContent,
		"refusal":     nil,
		"annotations": []interface{}{},
	}
	if strings.TrimSpace(reasoningContent) != "" {
		message["reasoning_content"] = reasoningContent
	}
	usage := addReasoningUsage(buildChatUsagePayload(req, finalContent, toolCalls), reasoningContent)
	resp := map[string]interface{}{
		"id":                 id,
		"object":             "chat.completion",
		"created":            time.Now().Unix(),
		"model":              model,
		"service_tier":       nil,
		"system_fingerprint": fingerprint,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       message,
				"finish_reason": "stop",
			},
		},
		"usage": usage,
	}
	if len(toolCalls) > 0 {
		choice := resp["choices"].([]map[string]interface{})[0]
		message := choice["message"].(map[string]interface{})
		message["tool_calls"] = toolCalls
		if strings.TrimSpace(finalContent) == "" {
			message["content"] = nil
		}
		choice["finish_reason"] = "tool_calls"
	}
	writeJSON(w, resp)
	h.auditChatOutcome(context.Background(), req.account, req, chatOutcome{Finish: "stop", Usage: usage, UsageSource: audit.UsageSourceEstimated})
}
