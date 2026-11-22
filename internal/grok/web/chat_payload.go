package web

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"strings"
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

func (h *Handler) uploadChatAttachments(ctx context.Context, token string, inputs []preparedChatAttachment) ([]string, error) {
	out := make([]string, 0, len(inputs))
	for _, item := range inputs {
		if item.document {
			id, err := h.webClient().uploadDirectAttachment(ctx, token, item.name, item.mime, item.data, "/", "")
			if err != nil {
				return nil, err
			}
			out = append(out, id)
			continue
		}
		id, uri, err := h.webClient().uploadFile(ctx, token, item.name, item.mime, base64.StdEncoding.EncodeToString(item.data))
		if err != nil {
			return nil, err
		}
		if id == "" {
			id = uri
		}
		out = append(out, id)
	}
	return uniqueStrings(out), nil
}
