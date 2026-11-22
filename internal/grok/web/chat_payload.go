package web

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"orchids-api/internal/httpclient"
	"strings"
	"time"
)

func (h *Handler) buildChatPayload(spec ModelSpec, text string, fileAttachments []string, req *ChatCompletionsRequest) map[string]any {
	payload := h.webClient().chatPayload(spec, text, true, 0)
	if len(fileAttachments) > 0 {
		payload["fileAttachments"] = fileAttachments
	}
	if req != nil {
		override := map[string]any{}
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
			responseMetadata, _ := payload["responseMetadata"].(map[string]any)
			if responseMetadata == nil {
				responseMetadata = map[string]any{}
				payload["responseMetadata"] = responseMetadata
			}
			modelConfigOverride, _ := responseMetadata["modelConfigOverride"].(map[string]any)
			if modelConfigOverride == nil {
				modelConfigOverride = map[string]any{}
				responseMetadata["modelConfigOverride"] = modelConfigOverride
			}
			maps.Copy(modelConfigOverride, override)
		}
	}

	return payload
}

func buildVideoCreatePayload(spec ModelSpec, text string, attachmentInputs []AttachmentInput, videoCfg VideoConfig) (map[string]any, error) {
	videoCfg.Normalize()
	for _, item := range attachmentInputs {
		if strings.EqualFold(strings.TrimSpace(item.Type), "image") && strings.TrimSpace(item.Data) != "" {
			return nil, fmt.Errorf("web video generation currently supports text-to-video only; image references are not supported")
		}
	}
	prompt := strings.TrimSpace(text)
	payload := map[string]any{
		"modelName":            firstNonEmpty(strings.TrimSpace(spec.UpstreamModel), "imagine-video-gen"),
		"message":              prompt + " --mode=custom",
		"enableImageStreaming": true,
		"enableSideBySide":     true,
		"sendFinalMetadata":    true,
		"responseMetadata": map[string]any{
			"experiments": []any{},
			"modelConfigOverride": map[string]any{
				"modelMap": map[string]any{},
			},
		},
		"mediaGenInput": map[string]any{
			"textToVideo": map[string]any{
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

func (h *Handler) uploadAttachmentInputs(ctx context.Context, token string, inputs []AttachmentInput) ([]string, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(inputs))
	remaining := maxChatDocumentBytes
	for _, item := range inputs {
		if item.Type == "file" {
			filename, mime, data, err := h.loadChatDocument(ctx, item, remaining)
			if err != nil {
				return nil, err
			}
			remaining -= len(data)
			id, err := h.webClient().uploadDirectAttachment(ctx, token, filename, mime, data, "/", "")
			if err != nil {
				return nil, err
			}
			out = append(out, id)
			continue
		}
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
		data, err = fetchRemoteAsDataURI(ctx, data, 30*time.Second, proxyFunc, 60<<20)
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
