package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"orchids-api/internal/httpserver"
	"orchids-api/internal/pricing"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

func (h *Handler) streamImageGeneration(w http.ResponseWriter, body io.Reader, token, prompt, format string, n int, publicBase string) {
	h.streamImageResults(w, body, token, prompt, format, n, publicBase, nil)
}

// streamImageEdit streams one edit call. editInputs are the uploaded input
// asset IDs, which the edit stream echoes back and which are not results.
func (h *Handler) streamImageEdit(w http.ResponseWriter, body io.Reader, token, prompt, format string, publicBase string, editInputs []string) {
	h.streamImageResults(w, body, token, prompt, format, 1, publicBase, editInputs)
}

// streamImageResults relays image progress and then the finished images. A
// non-nil editInputs marks an edit stream, whose previews and echoed inputs
// are dropped from the results.
func (h *Handler) streamImageResults(w http.ResponseWriter, body io.Reader, token, prompt, format string, n int, publicBase string, editInputs []string) {
	flusher := httpserver.StreamResponseHeaders(w)

	field := imageResponseField(format)
	var urls []string
	targetIndex := -1

	if err := parseUpstreamLines(body, func(resp map[string]any) error {
		if index, progress, ok := extractImageProgress(resp); ok {
			outIndex := index
			if n == 1 {
				if targetIndex < 0 {
					targetIndex = index
				}
				if index != targetIndex {
					return nil
				}
				outIndex = 0
			}
			data := map[string]any{
				"type":     "image_generation.partial_image",
				field:      "",
				"index":    outIndex,
				"progress": progress,
			}
			writeSSE(w, flusher, "image_generation.partial_image", util.EncodeJSONBytes(data))
		}
		if editInputs != nil {
			urls = appendImageEditResultURLs(urls, resp)
		} else {
			urls = appendImageResultURLs(urls, resp)
		}
		return nil
	}); err != nil {
		httpserver.WriteSSEStreamError(w, flusher, nil, "stream parse error: "+err.Error())
		return
	}

	if editInputs != nil {
		urls = withoutInputAssets(urls, editInputs)
	}
	urls = normalizeGeneratedImageURLs(urls, n)
	if len(urls) == 0 {
		writeSSECodedError(w, flusher, "no image generated", "no_image_generated")
		return
	}

	for i, u := range urls {
		val, err := h.imageOutputValue(context.Background(), token, u, format)
		if err != nil {
			// Always fatal: the caller gets this gateway's own absolute asset URL
			// or an error, never the upstream CDN address.
			slog.Warn("grok image stream convert failed", "url", u, "error", err)
			writeSSECodedError(w, flusher, "image cache failed: "+err.Error(), "image_cache_failed")
			return
		}
		if field == "url" {
			if publicBase == "" {
				writeSSECodedError(w, flusher, "image URL base is not configured", "image_url_base_missing")
				return
			}
			if strings.HasPrefix(val, "/") {
				val = publicBase + val
			}
		}
		data := map[string]any{
			"type":           "image_generation.completed",
			field:            val,
			"index":          i,
			"revised_prompt": "",
			"mime_type":      imageMimeTypeForValue(field, val),
			"usage":          buildImageUsagePayload(prompt, len(urls)),
		}
		writeSSE(w, flusher, "image_generation.completed", util.EncodeJSONBytes(data))
	}
	writeSSE(w, flusher, "", []byte("[DONE]"))
}

func (h *Handler) HandleImagesGenerations(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	var req ImagesGenerationsRequest
	if !httpserver.DecodeJSONBodyLimit(w, r, &req, maxGrokJSONBodyBytes) {
		return
	}
	req.Model = normalizeModelID(req.Model)
	req.Normalize()
	// grok2api treats a missing model as a validation failure; the local fallback
	// used to generate an image (and spend image quota) on a request that named no
	// model at all.
	if req.Model == "" {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if !httpserver.RequireAPIKeyModel(w, r, req.Model) {
		return
	}
	// Transport-layer validation, exactly as the reference performs it: a ratio is
	// a ratio, a size is one of the four accepted edit sizes, and a resolution is
	// 1k or 2k.
	if strings.TrimSpace(req.AspectRatio) != "" && !validImageAspectRatio(req.AspectRatio) {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_parameter", "aspect_ratio is not supported")
		return
	}
	if strings.TrimSpace(req.Size) != "" && !validImageEditSize(req.Size) {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_parameter", "size must be auto, 1024x1024, 1024x1536 or 1536x1024")
		return
	}
	resolution, resolutionErr := normalizeImageResolution(req.Resolution)
	if resolutionErr != nil {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_parameter", resolutionErr.Error())
		return
	}
	req.Resolution = resolution
	rawResponseFormat := strings.ToLower(strings.TrimSpace(req.ResponseFormat))
	if rawResponseFormat != "" && rawResponseFormat != "url" && rawResponseFormat != "b64_json" && rawResponseFormat != "base64" {
		httpserver.WriteError(w, http.StatusBadRequest, "response_format must be url or b64_json")
		return
	}
	req.ResponseFormat = normalizeImageResponseFormat(req.ResponseFormat)
	if !isImageGenerationModel(req.Model) {
		httpserver.WriteError(w, http.StatusBadRequest, "image generation model must be one of [grok-imagine-image-lite, grok-imagine-image, grok-imagine-image-2.0]")
		return
	}
	ratio, ratioErr := normalizeImageAspectRatio(req.AspectRatio, req.Size)
	if ratioErr != nil {
		writeGrokUpstreamError(w, ratioErr)
		return
	}
	req.AspectRatio = ratio
	req.Size = strings.ToLower(strings.TrimSpace(req.Size))
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		httpserver.WriteError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if req.N < 1 || req.N > 10 {
		httpserver.WriteError(w, http.StatusBadRequest, "n must be between 1 and 10")
		return
	}
	if req.Stream && req.N != 1 {
		httpserver.WriteError(w, http.StatusBadRequest, "Streaming is only supported with n=1.")
		return
	}
	if req.Stream && normalizeModelID(req.Model) == "grok-imagine-image-lite" {
		httpserver.WriteError(w, http.StatusBadRequest, "grok-imagine-image-lite does not support stream")
		return
	}
	if req.PartialImages != nil {
		if *req.PartialImages < 0 || *req.PartialImages > 3 {
			httpserver.WriteError(w, http.StatusBadRequest, "partial_images must be between 0 and 3")
			return
		}
		if *req.PartialImages > 0 && !req.Stream {
			httpserver.WriteError(w, http.StatusBadRequest, "partial_images requires stream=true")
			return
		}
	}
	quality := strings.ToLower(strings.TrimSpace(req.Quality))
	if quality != "" && quality != "low" && quality != "medium" {
		httpserver.WriteError(w, http.StatusBadRequest, "quality must be low or medium")
		return
	}
	req.Quality = quality
	if raw := strings.TrimSpace(string(req.StorageOptions)); raw != "" && raw != "null" {
		httpserver.WriteError(w, http.StatusBadRequest, "storage_options is not supported")
		return
	}
	h.serveImagesGenerations(r.Context(), w, req, detectPublicBaseURL(r))
}

func (h *Handler) serveImagesGenerations(ctx context.Context, w http.ResponseWriter, req ImagesGenerationsRequest, publicBase string) {
	if err := h.ensureModelCapability(ctx, req.Model, store.CapabilityImage); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, modelValidationMessage(req.Model, err))
		return
	}

	spec, ok := ResolveModel(req.Model)
	if !ok || !spec.IsImage || !isImageGenerationModel(spec.ID) {
		httpserver.WriteError(w, http.StatusBadRequest, fmt.Sprintf("The model `%s` is not supported for image generation. Supported: [grok-imagine-image-lite, grok-imagine-image, grok-imagine-image-2.0]", req.Model))
		return
	}
	spec = h.applyPersistedRoute(ctx, spec)

	var sess *chatAccountSession
	var err error
	sess, err = h.openChatAccountSessionForModel(ctx, spec)
	if err != nil {
		writeGrokNoAccountError(w, err)
		return
	}
	defer func() {
		sess.Close()
	}()

	if req.Stream {
		if normalizeModelID(spec.ID) == "grok-imagine-image-lite" {
			h.streamAppChatImagesGeneration(ctx, w, sess, spec, req, publicBase)
			return
		}
		h.streamImagineWSGeneration(ctx, w, sess, spec, req)
		return
	}
	if normalizeModelID(spec.ID) != "grok-imagine-image-lite" {
		h.collectImagineWSGeneration(ctx, w, sess, spec, req, publicBase)
		return
	}
	urls, err := h.collectAppChatImageURLs(ctx, sess, spec, req, true)
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	if len(urls) == 0 {
		httpserver.WriteError(w, http.StatusBadGateway, "no image generated")
		return
	}

	h.writeImageResults(w, ctx, sess.token, req.Prompt, urls, req.ResponseFormat, publicBase, true)
	if cost, priced := pricing.EstimateImageCost(spec.UpstreamModel, req.Resolution, req.Quality, len(urls)); priced {
		h.settleMediaBilling(ctx, req.Model, cost, map[string]any{
			"plane": "web", "images": len(urls), "resolution": req.Resolution,
		})
	}
}

func imagineWSProModel(modelID string) bool {
	switch normalizeModelID(modelID) {
	case "grok-imagine-image-2.0", "grok-imagine-image-quality":
		return true
	default:
		return false
	}
}

func imagePartialCount(req ImagesGenerationsRequest) int {
	if req.PartialImages == nil {
		return 0
	}
	return *req.PartialImages
}

// imagineNSFWAllowed decides whether a client-supplied `nsfw: true` may reach
// the upstream Imagine socket. The operator's configuration is the ceiling:
// a caller can never enable unfiltered generation on a deployment that turned
// it off (mirrors grok2api, where enable_nsfw comes from server config only).
func (h *Handler) imagineNSFWAllowed(requested *bool) bool {
	if requested == nil || !*requested {
		return false
	}
	if h == nil || h.configSnapshot() == nil {
		return false
	}
	return h.configSnapshot().PublicImagineNSFW()
}

func (h *Handler) streamImagineWSGeneration(ctx context.Context, w http.ResponseWriter, sess *chatAccountSession, spec ModelSpec, req ImagesGenerationsRequest) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	flusher := httpserver.StreamResponseHeaders(w)
	nsfw := h.imagineNSFWAllowed(req.NSFW)
	events, errs := h.streamImagineWSImages(ctx, sess, spec, req.Prompt, req.AspectRatio, req.N, nsfw)
	partialLimit := imagePartialCount(req)
	partialIndex := 0
	completed := 0
	for ev := range events {
		if ev.Type == "progress" && partialIndex < partialLimit && (strings.TrimSpace(ev.Blob) != "" || strings.TrimSpace(ev.URL) != "") {
			value, err := h.imagineImageOutputValue(ctx, sess.token, ev, "b64_json")
			if err == nil && strings.TrimSpace(value) != "" {
				outputFormat := imageOutputFormatFromBase64(value)
				writeSSE(w, flusher, "image_generation.partial_image", util.EncodeJSONBytes(map[string]any{
					"type": "image_generation.partial_image", "b64_json": value,
					"created_at": time.Now().Unix(), "size": imageEventSize("b64_json", value), "quality": "auto",
					"background": "auto", "output_format": outputFormat, "partial_image_index": partialIndex,
				}))
				partialIndex++
			}
		}
		if !ev.Final || (strings.TrimSpace(ev.Blob) == "" && strings.TrimSpace(ev.URL) == "") {
			continue
		}
		value, err := h.imagineImageOutputValue(ctx, sess.token, ev, "b64_json")
		if err != nil {
			httpserver.WriteSSEStreamError(w, flusher, nil, "image download failed: "+err.Error())
			return
		}
		outputFormat := imageOutputFormatFromBase64(value)
		writeSSE(w, flusher, "image_generation.completed", util.EncodeJSONBytes(map[string]any{
			"type": "image_generation.completed", "b64_json": value,
			"created_at": time.Now().Unix(), "size": imageEventSize("b64_json", value), "quality": "auto",
			"background": "auto", "output_format": outputFormat,
		}))
		completed++
	}
	err := <-errs
	if _, ok := errors.AsType[*imagineIncompleteError](err); ok {
		err = nil
	}
	if err != nil {
		httpserver.WriteSSEStreamError(w, flusher, nil, err.Error())
		return
	}
	if completed == 0 {
		writeSSECodedError(w, flusher, "no image generated", "no_image_generated")
		return
	}
	if completed < req.N {
		// The delivered images stay delivered; the stream still has to say
		// that the request was not fulfilled.
		writeSSECodedError(w, flusher, (&imagineIncompleteError{got: completed, want: req.N}).Error(), "image_generation_incomplete")
		return
	}
	writeSSE(w, flusher, "", []byte("[DONE]"))
}

func (h *Handler) collectImagineWSGeneration(ctx context.Context, w http.ResponseWriter, sess *chatAccountSession, spec ModelSpec, req ImagesGenerationsRequest, publicBase string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	nsfw := h.imagineNSFWAllowed(req.NSFW)
	events, errs := h.streamImagineWSImages(ctx, sess, spec, req.Prompt, req.AspectRatio, req.N, nsfw)
	field := imageResponseField(req.ResponseFormat)
	data := make([]map[string]any, 0, req.N)
	for ev := range events {
		if !ev.Final || (strings.TrimSpace(ev.Blob) == "" && strings.TrimSpace(ev.URL) == "") {
			continue
		}
		value, err := h.imagineImageOutputValue(ctx, sess.token, ev, req.ResponseFormat)
		if err != nil {
			writeGrokUpstreamError(w, err)
			return
		}
		if field == "url" {
			if publicBase == "" {
				httpserver.WriteErrorCode(w, http.StatusInternalServerError, "image_url_base_missing", "image URL base is not configured")
				return
			}
			if strings.HasPrefix(value, "/") {
				value = publicBase + value
			}
		}
		data = append(data, map[string]any{
			field:            value,
			"revised_prompt": "",
			"mime_type":      imageMimeTypeForValue(field, value),
		})
	}
	err := <-errs
	if _, ok := errors.AsType[*imagineIncompleteError](err); ok {
		err = nil
	}
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	if len(data) == 0 {
		httpserver.WriteError(w, http.StatusBadGateway, "no image generated")
		return
	}
	if len(data) < req.N {
		httpserver.WriteErrorCode(w, http.StatusBadGateway, "image_generation_incomplete", (&imagineIncompleteError{got: len(data), want: req.N}).Error())
		return
	}
	util.WriteJSON(w, map[string]any{
		"created": time.Now().Unix(), "data": data,
		"usage": buildImageUsagePayload(req.Prompt, len(data)),
	})
}

func (h *Handler) streamAppChatImagesGeneration(ctx context.Context, w http.ResponseWriter, sess *chatAccountSession, spec ModelSpec, req ImagesGenerationsRequest, publicBase string) {
	onePayload := h.webClient().appChatImagePayload(spec, req.Prompt, req.Size, req.N)
	ensureImageNSFW(onePayload)
	resp, err := h.doAppChatImageRequest(ctx, sess, spec, &onePayload, true)
	if err != nil {
		slog.Warn("grok app-chat image stream upstream failed",
			"model", req.Model,
			"status", parseUpstreamStatus(err),
			"error", err,
		)
		writeGrokUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	h.syncGrokQuota(sess.acc, resp.Header)
	h.streamImageGeneration(w, resp.Body, sess.token, req.Prompt, req.ResponseFormat, req.N, publicBase)
	if cost, priced := pricing.EstimateImageCost(spec.UpstreamModel, req.Resolution, req.Quality, req.N); priced {
		h.settleMediaBilling(ctx, req.Model, cost, map[string]any{
			"plane": "web", "stream": true, "images": req.N,
		})
	}
}

func (h *Handler) collectAppChatImageURLs(ctx context.Context, sess *chatAccountSession, spec ModelSpec, req ImagesGenerationsRequest, allowSwitch bool) ([]string, error) {
	var urls []string
	var debugShapes []string
	var debugNoImage []string

	// A lite call is a fast chat that always draws two pictures, and only the
	// first is the drawing of the prompt: the second is often unrelated to it.
	// grok2api keeps the first and repeats the call for each image ("Lite 上游固定
	// 生成两张……按旧协议取首张并为 n 重复查询"); so does this collector. Each call
	// costs one fast query either way.
	maxAttempts := max(req.N*2, 4)
	deadline := time.Now().Add(max(60*time.Second, time.Duration(req.N)*40*time.Second))
	excludedAccountIDs := make([]int64, 0, maxAttempts)
	for i := range maxAttempts {
		if len(urls) >= req.N {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		imageLimitHit := false
		var callURLs []string
		payload := h.webClient().appChatImagePayload(spec, req.Prompt, req.Size, 1)
		ensureImageNSFW(payload)
		resp, err := h.doAppChatImageRequest(ctx, sess, spec, &payload, allowSwitch)
		if err != nil {
			slog.Warn("grok app-chat image upstream failed",
				"model", req.Model,
				"status", parseUpstreamStatus(err),
				"error", err,
			)
			return nil, err
		}
		h.syncGrokQuota(sess.acc, resp.Header)
		err = parseUpstreamLines(resp.Body, func(line map[string]any) error {
			if len(debugShapes) < 20 {
				debugShapes = append(debugShapes, imageDebugShape(line))
			}
			if len(debugNoImage) < 20 {
				debugNoImage = append(debugNoImage, appChatImageNoImageDiagnostics(line)...)
			}
			if isAppChatImageLimitResponse(line) {
				imageLimitHit = true
			}
			callURLs = append(callURLs, extractAppChatImageURLs(line)...)
			return nil
		})
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("stream parse error: %w", err)
		}
		if first := firstLiteImage(callURLs, urls); first != "" {
			urls = append(urls, first)
		}
		if len(urls) == 0 && imageLimitHit {
			err := fmt.Errorf("grok upstream status=429 body=image generation limit reached")
			if sess.acc != nil && sess.acc.GrokWebQuota.Weekly.HasLimit {
				// Paid: the weekly pool is spent; the account still chats.
				h.noteMediaExhausted(ctx, sess.acc, mediaLite)
			} else {
				h.markAccountStatus(ctx, sess.acc, err)
			}
			if !allowSwitch || sess == nil || i == maxAttempts-1 {
				return nil, err
			}
			if sess.acc != nil && sess.acc.ID != 0 {
				if !slices.Contains(excludedAccountIDs, sess.acc.ID) {
					excludedAccountIDs = append(excludedAccountIDs, sess.acc.ID)
				}
				sess.Close()
				next, switchErr := h.openChatAccountSessionForModelExcluding(ctx, excludedAccountIDs, spec)
				if switchErr != nil {
					// The switch failed, so the upgrade-limit error above is the one
					// the client acts on — but the pool's reason must not vanish: it is
					// the only record of why another account could not be found.
					slog.Warn("No image account to switch to after the generation limit",
						"error", switchErr, "account_id", sess.acc.ID)
					return nil, err
				}
				sess.acc = next.acc
				sess.token = next.token
				sess.poolCandidates = next.poolCandidates
				sess.release = next.release
			}
		}
	}
	urls = normalizeGeneratedImageURLs(urls, req.N)
	if len(urls) == 0 {
		slog.Warn("grok image generation returned no images",
			"model", req.Model,
			"attempts", maxAttempts,
			"event_shapes", uniqueStrings(debugShapes),
			"diagnostics", uniqueStrings(debugNoImage),
		)
		return nil, fmt.Errorf("no image generated")
	}
	return urls, nil
}

func (h *Handler) doAppChatImageRequest(ctx context.Context, sess *chatAccountSession, spec ModelSpec, payload *map[string]any, allowSwitch bool) (*http.Response, error) {
	if payload == nil {
		return nil, fmt.Errorf("empty payload")
	}
	if normalizeModelID(spec.ID) == "grok-imagine-image-lite" {
		if allowSwitch {
			return h.doAutoSwitchRequest(ctx, sess, payload, nil, (*Client).doChat)
		}
		return h.doSingleAccountRequest(ctx, sess, *payload, markAllGrokAccountStatuses, (*Client).doChat)
	}
	if allowSwitch {
		return h.doAutoSwitchRequest(ctx, sess, payload, nil, (*Client).doAppChatCreateAndRespond)
	}
	return h.doSingleAccountRequest(ctx, sess, *payload, markAllGrokAccountStatuses, (*Client).doAppChatCreateAndRespond)
}

// firstLiteImage returns the first finished image of one lite call, in the
// order the stream delivered it, skipping one already taken by an earlier call.
func firstLiteImage(callURLs, taken []string) string {
	for _, candidate := range callURLs {
		candidate = strings.TrimSpace(candidate)
		if imageURLScore(candidate) < 0 || slices.Contains(taken, candidate) {
			continue
		}
		return candidate
	}
	return ""
}

// imageURLShape describes an image URL for the log without its identifiers:
// the host and the path, with every segment that looks like an ID elided, so
// "users/…/generated/…/image.jpg" still tells a drawing from anything else.
func imageURLShape(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || raw == "" {
		return ""
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i, segment := range segments {
		if len(segment) >= 16 || (len(segment) >= 8 && strings.ContainsAny(segment, "0123456789")) {
			segments[i] = "…"
		}
	}
	return parsed.Host + "/" + strings.Join(segments, "/")
}
