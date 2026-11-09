package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/httpserver"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

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
		httpserver.WriteError(w, http.StatusBadRequest, "image generation model must be one of [grok-imagine-image, grok-imagine-image-2.0]")
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
		httpserver.WriteError(w, http.StatusBadRequest, fmt.Sprintf("The model `%s` is not supported for image generation. Supported: [grok-imagine-image, grok-imagine-image-2.0]", req.Model))
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
		h.streamImagineWSGeneration(ctx, w, sess, spec, req)
		return
	}
	h.collectImagineWSGeneration(ctx, w, sess, spec, req, publicBase)
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
	events, errs := h.streamImagineWSImages(ctx, sess, req.Prompt, req.AspectRatio, req.N, nsfw, imagineWSProModel(spec.ID))
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
	if err := <-errs; err != nil {
		httpserver.WriteSSEStreamError(w, flusher, nil, err.Error())
		return
	}
	if completed == 0 {
		writeSSECodedError(w, flusher, "no image generated", "no_image_generated")
		return
	}
	writeSSE(w, flusher, "", []byte("[DONE]"))
}

func (h *Handler) collectImagineWSGeneration(ctx context.Context, w http.ResponseWriter, sess *chatAccountSession, spec ModelSpec, req ImagesGenerationsRequest, publicBase string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	nsfw := h.imagineNSFWAllowed(req.NSFW)
	events, errs := h.streamImagineWSImages(ctx, sess, req.Prompt, req.AspectRatio, req.N, nsfw, imagineWSProModel(spec.ID))
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
	if err := <-errs; err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	if len(data) == 0 {
		httpserver.WriteError(w, http.StatusBadGateway, "no image generated")
		return
	}
	util.WriteJSON(w, map[string]any{
		"created": time.Now().Unix(), "data": data,
		"usage": buildImageUsagePayload(req.Prompt, len(data)),
	})
}
