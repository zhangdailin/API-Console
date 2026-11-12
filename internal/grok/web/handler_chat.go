package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/httpserver"
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

func appendUsage(dst []byte, usage map[string]any) []byte {
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
func appendChatCompletionChunkFinish(dst []byte, finish string, hasFinish bool, usage map[string]any) []byte {
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

func appendChatCompletionChunkWithUsage(dst []byte, id string, created int64, model, fingerprint, role, content string, finish string, hasFinish bool, usage map[string]any) []byte {
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

func appendChatCompletionSnapshotChunkWithUsage(dst []byte, id string, created int64, model, fingerprint, messageContent string, finish string, hasFinish bool, usage map[string]any) []byte {
	dst = appendChatCompletionChunkPrefix(dst, id, created, model, fingerprint)
	dst = append(dst, `,"choices":[{"index":0,"delta":{},"message":{"role":"assistant","content":`...)
	dst = strconv.AppendQuote(dst, messageContent)
	dst = append(dst, '}')
	return appendChatCompletionChunkFinish(dst, finish, hasFinish, usage)
}

func appendChatCompletionToolCallsChunkWithUsage(dst []byte, id string, created int64, model, fingerprint string, toolCalls []map[string]any, finish string, hasFinish bool, usage map[string]any) []byte {
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

func isThinkingResponse(resp map[string]any) bool {
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
		val, err := chatwire.ParseLooseBoolAnyForField(raw, path[len(path)-1])
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
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	var req ChatCompletionsRequest
	if !httpserver.DecodeJSONBodyLimit(w, r, &req, maxGrokJSONBodyBytes) {
		return
	}
	req.StartedAt = time.Now()
	req.SourceOperation, _ = r.Context().Value(chatSourceOperationKey{}).(string)
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
	if !httpserver.RequireAPIKeyModel(w, r, req.Model) {
		return
	}
	session := grokbase.SessionFromContext(r.Context())
	if session.Key == "" {
		session = grokbase.PrepareSession(r, req.Model, req.PromptCacheKey, req.Messages)
	}
	if session.Key != "" {
		req.PromptCacheKey = session.Key
		req.ReasoningReplay = session.Replay
		r = r.WithContext(grokbase.WithSession(r.Context(), session))
	}
	spec, ok := h.resolveConversationModel(r.Context(), req.Model)
	if !ok {
		httpserver.WriteErrorCode(w, http.StatusNotFound, "model_not_found", modelNotFoundMessage(req.Model))
		return
	}
	if spec.AliasReasoningEffort != "" {
		effort := spec.AliasReasoningEffort
		req.ReasoningEffort = &effort
	}
	if err := h.ensureResolvedModelCapability(r.Context(), spec.ID, spec, store.CapabilityChat); err != nil {
		if strings.EqualFold(strings.TrimSpace(err.Error()), "model not found") {
			httpserver.WriteErrorCode(w, http.StatusNotFound, "model_not_found", modelNotFoundMessage(req.Model))
			return
		}
		httpserver.WriteError(w, http.StatusBadRequest, modelValidationMessage(req.Model, err))
		return
	}
	if !spec.SupportsConversation() {
		httpserver.WriteError(w, http.StatusBadRequest, fmt.Sprintf("model %s does not support chat completions", req.Model))
		return
	}

	publicBase := detectPublicBaseURL(r)
	if spec.IsImage {
		prompt, imageURLs := extractPromptAndImageURLs(req.Messages)
		prompt = strings.TrimSpace(prompt)
		if prompt == "" {
			httpserver.WriteError(w, http.StatusBadRequest, "prompt is required")
			return
		}
		imageCfg := req.ImageConfig
		if imageCfg == nil {
			imageCfg = &ImageConfig{}
			imageCfg.Normalize()
		}
		if imageCfg.N < 1 || imageCfg.N > 10 {
			httpserver.WriteError(w, http.StatusBadRequest, "image_config.n must be between 1 and 10")
			return
		}
		// The same transport-layer rules the image endpoints apply: a ratio is a
		// ratio, and a resolution tier is 1k or 2k.
		if edgeAspect := strings.TrimSpace(imageCfg.AspectRatio); edgeAspect != "" && !validImageAspectRatio(edgeAspect) {
			httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_parameter", "aspect_ratio is not supported")
			return
		}
		if _, err := normalizeImageResolution(imageCfg.Resolution); err != nil {
			httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_parameter", err.Error())
			return
		}
		if isImageGenerationModel(req.Model) && normalizeModelID(req.Model) == "grok-imagine-image-lite" && imageCfg.N > 4 {
			httpserver.WriteError(w, http.StatusBadRequest, "image_config.n must be between 1 and 4 for grok-imagine-image-lite")
			return
		}
		if isImageEditModel(req.Model) && imageCfg.N > 2 {
			httpserver.WriteError(w, http.StatusBadRequest, "image_config.n must be between 1 and 2 for image edit")
			return
		}
		if req.Stream && imageCfg.N > 2 {
			httpserver.WriteError(w, http.StatusBadRequest, "streaming is only supported when image_config.n=1 or n=2")
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
				httpserver.WriteError(w, http.StatusBadRequest, "image_url is required for image edits")
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
		httpserver.WriteError(w, http.StatusBadRequest,
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
		httpserver.WriteError(w, http.StatusBadRequest, "empty message")
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

	buildPayload := func(token string) (map[string]any, error) {
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

// NOTE: streamMarkupFilter.feed is implemented earlier in this file.
