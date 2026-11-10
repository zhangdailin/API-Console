package web

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"encoding/json"

	"orchids-api/internal/httpserver"
	"orchids-api/internal/pricing"
	"orchids-api/internal/store"
)

var imageEditPlaceholderRE = regexp.MustCompile(`(?i)@IMAGE(\d+)\b`)

// The Web edit wire format follows grok2api's mediaGenInput.imageToImage
// contract. inputAssets are metadata IDs, not download URLs.
func (h *Handler) buildImageEditPayload(spec ModelSpec, prompt string, assets []string, aspectRatio string) map[string]any {
	input := map[string]any{"prompt": strings.TrimSpace(prompt), "inputAssets": assets}
	if aspectRatio != "" {
		input["aspectRatio"] = aspectRatio
	}
	payload := map[string]any{
		"modelName": spec.UpstreamModel, "message": strings.TrimSpace(prompt),
		"enableImageStreaming": true, "enableSideBySide": true, "sendFinalMetadata": true,
		"mediaGenInput": map[string]any{"imageToImage": input},
	}
	if h != nil && h.configSnapshot() != nil {
		payload["temporary"] = h.configSnapshot().GrokChatTemporary()
		payload["disableMemory"] = h.configSnapshot().GrokChatDisableMemory(false)
		if instruction := h.configSnapshot().GrokChatCustomInstruction(); instruction != "" {
			payload["customPersonality"] = instruction
		}
	}
	return payload
}

func (h *Handler) buildImageEditRequestPayload(ctx context.Context, token string, spec ModelSpec, prompt string, inputs []imageEditUploadInput, ratio string) (map[string]any, error) {
	values := make([]string, 0, len(inputs))
	for _, input := range inputs {
		values = append(values, dataURIFromBytes(input.mime, input.data))
	}
	return h.buildImageEditPayloadFromInputs(ctx, token, spec, prompt, values, ratio)
}

func (h *Handler) buildImageEditPayloadFromInputs(ctx context.Context, token string, spec ModelSpec, prompt string, inputs []string, ratio string) (map[string]any, error) {
	refs := make([]imageEditReference, 0, len(inputs))
	assets := make([]string, 0, len(inputs))
	for _, input := range inputs {
		fileID, err := h.uploadImageEditInput(ctx, token, input)
		if err != nil {
			return nil, fmt.Errorf("image upload failed: %w", err)
		}
		fileID = strings.TrimSpace(fileID)
		if fileID == "" {
			return nil, fmt.Errorf("image upload returned no fileMetadataId")
		}
		assets = append(assets, fileID)
		refs = append(refs, imageEditReference{fileID: fileID})
	}
	if len(assets) == 0 {
		return nil, fmt.Errorf("image_url is required for image edits")
	}
	return h.buildImageEditPayload(spec, replaceImageEditPlaceholders(prompt, refs), assets, ratio), nil
}

func replaceImageEditPlaceholders(prompt string, refs []imageEditReference) string {
	if len(refs) == 0 || !strings.Contains(strings.ToUpper(prompt), "@IMAGE") {
		return prompt
	}
	return imageEditPlaceholderRE.ReplaceAllStringFunc(prompt, func(match string) string {
		groups := imageEditPlaceholderRE.FindStringSubmatch(match)
		if len(groups) != 2 {
			return match
		}
		idx, err := strconv.Atoi(groups[1])
		if err != nil || idx < 1 || idx > len(refs) {
			return match
		}
		fileID := strings.TrimSpace(refs[idx-1].fileID)
		if fileID == "" {
			return match
		}
		return "@" + fileID
	})
}

func (h *Handler) handleChatImageEdit(
	ctx context.Context,
	w http.ResponseWriter,
	req ChatCompletionsRequest,
	spec ModelSpec,
	prompt string,
	imageURLs []string,
	publicBase string,
) {
	if len(imageURLs) == 0 {
		httpserver.WriteError(w, http.StatusBadRequest, "image_url is required for image edits")
		return
	}
	if len(imageURLs) > 7 {
		imageURLs = imageURLs[len(imageURLs)-7:]
	}

	imageCfg := req.ImageConfig
	if imageCfg == nil {
		imageCfg = &ImageConfig{}
	}
	imageCfg.Normalize()
	n := max(imageCfg.N, 1)
	if n > 2 {
		httpserver.WriteError(w, http.StatusBadRequest, "image_config.n must be between 1 and 2 for image edit")
		return
	}
	// One edit call produces one image, and a stream relays one call.
	if req.Stream && n > 1 {
		httpserver.WriteError(w, http.StatusBadRequest, "streaming image edits support image_config.n=1 only")
		return
	}
	responseFormat := normalizeImageResponseFormat(imageCfg.ResponseFormat)
	if _, err := normalizeImageEditSize(imageCfg.Size); err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	// grok2api validates these at the transport layer before the provider maps a
	// size to a ratio, so a caller cannot smuggle a pixel string through
	// aspect_ratio and an unsupported resolution is a parameter error.
	// Transport-layer validation, as grok2api performs it before the provider
	// maps a size to a ratio.
	if edgeAspect := strings.TrimSpace(imageCfg.AspectRatio); edgeAspect != "" && !validImageAspectRatio(edgeAspect) {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_parameter", "aspect_ratio is not supported")
		return
	}
	if _, err := normalizeImageResolution(imageCfg.Resolution); err != nil {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}
	ratio, _ := normalizeImageAspectRatio(imageCfg.AspectRatio, imageCfg.Size)

	sess, err := h.openChatAccountSessionForModel(ctx, spec)
	if err != nil {
		writeGrokNoAccountError(w, err)
		return
	}
	defer sess.Close()

	rawPayload, err := h.buildImageEditPayloadFromInputs(ctx, sess.token, spec, prompt, imageURLs, ratio)
	if err != nil {
		if skipExternalAttachmentFetchGrokAccountStatus(err) {
			h.markAccountStatus(ctx, sess.acc, err)
		}
		writeGrokUpstreamError(w, err)
		return
	}
	rebuildPayload := func(token string) (map[string]any, error) {
		return h.buildImageEditPayloadFromInputs(ctx, token, spec, prompt, imageURLs, ratio)
	}

	if req.Stream {
		resp, err := h.doChatWithAutoSwitchRebuild(ctx, sess, &rawPayload, rebuildPayload)
		if err != nil {
			writeGrokUpstreamError(w, err)
			return
		}
		defer resp.Body.Close()
		h.syncGrokQuota(sess.acc, resp.Header)
		h.streamImageEdit(w, resp.Body, sess.token, prompt, responseFormat, publicBase, nonNilStrings(imageEditInputAssets(rawPayload)))
		if cost, priced := pricing.EstimateImageEditCost(spec.UpstreamModel, imageCfg.Resolution, "", n, len(imageURLs)); priced {
			h.settleMediaBilling(ctx, req.Model, cost, map[string]any{
				"plane": "web", "stream": true, "images": n, "input_images": len(imageURLs),
			})
		}
		return
	}

	urls, ok := h.collectImageChatURLs(ctx, w, sess, &rawPayload, rebuildPayload, n)
	if !ok {
		return
	}

	h.writeImageResults(w, ctx, sess.token, prompt, urls, responseFormat, publicBase, false)
}

func isAllowedEditImageMime(mime string) bool {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png", "image/jpeg", "image/webp":
		return true
	default:
		return false
	}
}

type imageEditJSONInput struct {
	URL    string `json:"url"`
	FileID string `json:"file_id"`
}

type imageEditJSONRequest struct {
	Model          string               `json:"model"`
	Prompt         string               `json:"prompt"`
	Image          *imageEditJSONInput  `json:"image"`
	Images         []imageEditJSONInput `json:"images"`
	N              int                  `json:"n"`
	Size           string               `json:"size"`
	AspectRatio    string               `json:"aspect_ratio"`
	Resolution     string               `json:"resolution"`
	Quality        string               `json:"quality"`
	ResponseFormat string               `json:"response_format"`
	Stream         bool                 `json:"stream"`
	PartialImages  int                  `json:"partial_images"`
}

func decodeImageEditJSON(w http.ResponseWriter, r *http.Request) (*imageEditJSONRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 80<<20)
	decoder := json.NewDecoder(r.Body)
	var request imageEditJSONRequest
	if err := decoder.Decode(&request); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid image edit JSON request")
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		httpserver.WriteError(w, http.StatusBadRequest, "image edit request must contain one JSON object")
		return nil, false
	}
	return &request, true
}

func imageEditJSONInputs(request *imageEditJSONRequest) ([]imageEditJSONInput, error) {
	inputs := make([]imageEditJSONInput, 0, 1+len(request.Images))
	if request.Image != nil {
		inputs = append(inputs, *request.Image)
	}
	inputs = append(inputs, request.Images...)
	for index, input := range inputs {
		urlValue := strings.TrimSpace(input.URL)
		fileID := strings.TrimSpace(input.FileID)
		if (urlValue == "") == (fileID == "") {
			return nil, fmt.Errorf("image input %d must provide exactly one of url or file_id", index+1)
		}
		if fileID != "" && !validMediaInputID(fileID) {
			return nil, fmt.Errorf("image input %d file_id is invalid", index+1)
		}
		if urlValue != "" && !isRemoteURL(urlValue) && !strings.HasPrefix(strings.ToLower(urlValue), "data:image/") {
			return nil, fmt.Errorf("image input %d url must be an HTTP(S) URL or image data URL", index+1)
		}
	}
	return inputs, nil
}

func (h *Handler) resolveImageEditJSONInputs(ctx context.Context, inputs []imageEditJSONInput, owner string) ([]string, error) {
	resolved := make([]string, 0, len(inputs))
	var resolvedBytes int64
	for index, input := range inputs {
		value := strings.TrimSpace(input.URL)
		if fileID := strings.TrimSpace(input.FileID); fileID != "" {
			var size int64
			var err error
			value, size, err = h.resolveMediaInputDataURL(ctx, fileID, owner, "image")
			if err != nil {
				return nil, fmt.Errorf("image input %d file_id: %w", index+1, err)
			}
			resolvedBytes += size
			if resolvedBytes > maxResolvedMediaBytes {
				return nil, fmt.Errorf("combined file_id media exceeds 32 MiB")
			}
		}
		resolved = append(resolved, value)
	}
	return resolved, nil
}

func (h *Handler) HandleImagesEdits(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	mediaType, _, _ := mime.ParseMediaType(strings.TrimSpace(r.Header.Get("Content-Type")))
	jsonRequest := (*imageEditJSONRequest)(nil)
	if strings.EqualFold(mediaType, "application/json") {
		var ok bool
		jsonRequest, ok = decodeImageEditJSON(w, r)
		if !ok {
			return
		}
	} else if err := r.ParseMultipartForm(80 << 20); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}
	formValue := func(name string) string {
		if jsonRequest == nil {
			return r.FormValue(name)
		}
		switch name {
		case "model":
			return jsonRequest.Model
		case "prompt":
			return jsonRequest.Prompt
		case "size":
			return jsonRequest.Size
		case "aspect_ratio":
			return jsonRequest.AspectRatio
		case "resolution":
			return jsonRequest.Resolution
		case "quality":
			return jsonRequest.Quality
		case "response_format":
			return jsonRequest.ResponseFormat
		}
		return ""
	}

	prompt := strings.TrimSpace(formValue("prompt"))
	if prompt == "" {
		httpserver.WriteError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	model := strings.TrimSpace(formValue("model"))
	if model == "" {
		model = "grok-imagine-image-edit"
	}
	model = normalizeModelID(model)
	if !httpserver.RequireAPIKeyModel(w, r, model) {
		return
	}
	// Grok Web edits through one route. A client that names a generation
	// model (the tools page names the one its quality tier selected) means an
	// edit of the supplied images, so it is served by the edit route.
	if generation, known := ResolveModel(model); known && generation.IsImage && !isImageEditModel(model) {
		model = "grok-imagine-image-edit"
	}
	spec, ok := ResolveModel(model)
	if !isImageEditModel(model) {
		httpserver.WriteError(w, http.StatusBadRequest, "image edit model must be grok-imagine-image-edit")
		return
	}
	if jsonRequest == nil && r.MultipartForm != nil && len(r.MultipartForm.File["mask"]) > 0 {
		httpserver.WriteError(w, http.StatusBadRequest, "mask is not supported yet")
		return
	}
	n := parseIntLoose(formValue("n"), 1)
	if jsonRequest != nil && jsonRequest.N != 0 {
		n = jsonRequest.N
	}
	maxN := 2
	if n < 1 || n > maxN {
		httpserver.WriteError(w, http.StatusBadRequest, fmt.Sprintf("n must be between 1 and %d for image edit", maxN))
		return
	}
	if edgeAspect := strings.TrimSpace(formValue("aspect_ratio")); edgeAspect != "" && !validImageAspectRatio(edgeAspect) {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_parameter", "aspect_ratio is not supported")
		return
	}
	if _, err := normalizeImageResolution(formValue("resolution")); err != nil {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}
	if _, err := normalizeImageEditSize(formValue("size")); err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	if _, err := normalizeImageAspectRatio(formValue("aspect_ratio"), formValue("size")); err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	stream := parseBoolLoose(formValue("stream"), false)
	if jsonRequest != nil {
		stream = jsonRequest.Stream
	}
	// One edit call produces one image, and a stream relays one call.
	if stream && n > 1 {
		httpserver.WriteError(w, http.StatusBadRequest, "streaming image edits support n=1 only")
		return
	}
	rawResponseFormat := strings.ToLower(strings.TrimSpace(formValue("response_format")))
	if rawResponseFormat != "" && rawResponseFormat != "url" && rawResponseFormat != "b64_json" && rawResponseFormat != "base64" {
		httpserver.WriteError(w, http.StatusBadRequest, "response_format must be url or b64_json")
		return
	}
	responseFormat := normalizeImageResponseFormat(rawResponseFormat)
	publicBase := detectPublicBaseURL(r)

	if !ok || !spec.IsImage || !isImageEditModel(spec.ID) {
		httpserver.WriteError(w, http.StatusBadRequest, "image edit model is not supported")
		return
	}
	spec = h.applyPersistedRoute(r.Context(), spec)
	if err := h.ensureModelCapability(r.Context(), model, store.CapabilityImageEdit); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, modelValidationMessage(model, err))
		return
	}

	var uploads []imageEditUploadInput
	var inputValues []string
	if jsonRequest != nil {
		jsonInputs, err := imageEditJSONInputs(jsonRequest)
		if err != nil {
			httpserver.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(jsonInputs) == 0 {
			httpserver.WriteError(w, http.StatusBadRequest, "image is required")
			return
		}
		if len(jsonInputs) > 7 {
			jsonInputs = jsonInputs[len(jsonInputs)-7:]
		}
		inputValues, err = h.resolveImageEditJSONInputs(r.Context(), jsonInputs, videoRequestOwner(r))
		if err != nil {
			httpserver.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		files := r.MultipartForm.File["image"]
		if len(files) == 0 {
			files = r.MultipartForm.File["image[]"]
		}
		if len(files) == 0 {
			httpserver.WriteError(w, http.StatusBadRequest, "image is required")
			return
		}
		if len(files) > 7 {
			files = files[len(files)-7:]
		}

		uploads = make([]imageEditUploadInput, 0, len(files))
		for _, fh := range files {
			file, err := fh.Open()
			if err != nil {
				httpserver.WriteError(w, http.StatusBadRequest, "failed to read image file")
				return
			}
			data, err := io.ReadAll(io.LimitReader(file, maxEditImageBytes+1))
			file.Close()
			if err != nil {
				httpserver.WriteError(w, http.StatusBadRequest, "failed to read image file")
				return
			}
			if len(data) == 0 {
				httpserver.WriteError(w, http.StatusBadRequest, "file content is empty")
				return
			}
			if len(data) > maxEditImageBytes {
				httpserver.WriteError(w, http.StatusBadRequest, "image file too large. maximum is 50MB")
				return
			}
			mimeType := strings.ToLower(strings.TrimSpace(fh.Header.Get("Content-Type")))
			if mimeType == "image/jpg" {
				mimeType = "image/jpeg"
			}
			if !isAllowedEditImageMime(mimeType) {
				mimeType = mimeFromFilename(strings.TrimSpace(fh.Filename))
				if mimeType == "image/jpg" {
					mimeType = "image/jpeg"
				}
			}
			if !isAllowedEditImageMime(mimeType) {
				httpserver.WriteError(w, http.StatusBadRequest, "unsupported image type. supported: png, jpg, webp")
				return
			}
			uploads = append(uploads, imageEditUploadInput{mime: mimeType, data: data})
		}
	}

	sess, err := h.openChatAccountSessionForModel(r.Context(), spec)
	if err != nil {
		writeGrokNoAccountError(w, err)
		return
	}
	defer sess.Close()

	ratio, _ := normalizeImageAspectRatio(formValue("aspect_ratio"), formValue("size"))
	var rawPayload map[string]any
	if jsonRequest != nil {
		rawPayload, err = h.buildImageEditPayloadFromInputs(r.Context(), sess.token, spec, prompt, inputValues, ratio)
	} else {
		rawPayload, err = h.buildImageEditRequestPayload(r.Context(), sess.token, spec, prompt, uploads, ratio)
	}
	if err != nil {
		if skipExternalAttachmentFetchGrokAccountStatus(err) {
			h.markAccountStatus(r.Context(), sess.acc, err)
		}
		writeGrokUpstreamError(w, err)
		return
	}
	rebuildPayload := func(token string) (map[string]any, error) {
		if jsonRequest != nil {
			return h.buildImageEditPayloadFromInputs(r.Context(), token, spec, prompt, inputValues, ratio)
		}
		return h.buildImageEditRequestPayload(r.Context(), token, spec, prompt, uploads, ratio)
	}

	if stream {
		resp, err := h.doChatWithAutoSwitchRebuild(r.Context(), sess, &rawPayload, rebuildPayload)
		if err != nil {
			writeGrokUpstreamError(w, err)
			return
		}
		defer resp.Body.Close()
		h.syncGrokQuota(sess.acc, resp.Header)
		h.streamImageEdit(w, resp.Body, sess.token, prompt, responseFormat, publicBase, nonNilStrings(imageEditInputAssets(rawPayload)))
		inputs := len(inputValues)
		if inputs == 0 {
			inputs = len(uploads)
		}
		if cost, priced := pricing.EstimateImageEditCost(spec.UpstreamModel, formValue("resolution"), formValue("quality"), n, inputs); priced {
			h.settleMediaBilling(r.Context(), model, cost, map[string]any{
				"plane": "web", "stream": true, "images": n, "input_images": inputs,
			})
		}
		return
	}

	urls, ok := h.collectImageChatURLs(r.Context(), w, sess, &rawPayload, rebuildPayload, n)
	if !ok {
		return
	}

	h.writeImageResults(w, r.Context(), sess.token, prompt, urls, responseFormat, publicBase, false)
}
