package web

import (
	"fmt"
	"strings"
)

func extractImageProgress(resp map[string]any) (index int, progress int, ok bool) {
	raw := mapAtAnyPath(resp,
		[]string{"streamingImageGenerationResponse"},
		[]string{"streaming_image_generation_response"},
		[]string{"imageGenerationProgress"},
		[]string{"image_generation_progress"},
		[]string{"modelResponse", "streamingImageGenerationResponse"},
		[]string{"modelResponse", "streaming_image_generation_response"},
	)
	if raw == nil {
		return 0, 0, false
	}
	return intAtAnyPath(raw,
			[]string{"imageIndex"},
			[]string{"image_index"},
			[]string{"index"},
		),
		intAtAnyPath(raw,
			[]string{"progress"},
			[]string{"percent"},
			[]string{"percentage"},
			[]string{"completedPercentage"},
		), true
}

func isAppChatImageLimitResponse(resp map[string]any) bool {
	if resp == nil {
		return false
	}
	response := resp
	if nested := mapAtAnyPath(resp, []string{"result", "response"}); nested != nil {
		response = nested
	}
	for _, path := range [][]string{
		{"error", "renderToolRateLimited"},
		{"userResponse", "streamErrors"},
		{"modelResponse", "streamErrors"},
		{"streamErrors"},
		{"errors"},
	} {
		if strings.Contains(strings.ToLower(diagnosticValueSummary(valueAtPath(response, path...))), "rendertoolratelimited") {
			return true
		}
	}
	for _, msg := range []string{
		stringAtAnyPath(response, []string{"error", "message"}),
		stringAtAnyPath(response, []string{"modelResponse", "message"}),
	} {
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "image generation limit") || strings.Contains(lower, "try again later") ||
			strings.Contains(lower, "media generation credits") {
			return true
		}
	}
	return false
}

func extractVideoProgress(resp map[string]any) (progress int, videoURL string, ok bool) {
	raw := mapAtAnyPath(resp,
		[]string{"streamingVideoGenerationResponse"},
		[]string{"streaming_video_generation_response"},
		[]string{"videoGenerationProgress"},
		[]string{"video_generation_progress"},
		[]string{"modelResponse", "streamingVideoGenerationResponse"},
		[]string{"modelResponse", "streaming_video_generation_response"},
	)
	if raw == nil {
		return 0, "", false
	}
	return intAtAnyPath(raw,
			[]string{"progress"},
			[]string{"percent"},
			[]string{"percentage"},
			[]string{"completedPercentage"},
		),
		stringAtAnyPath(raw,
			[]string{"videoUrl"},
			[]string{"videoURL"},
			[]string{"video_url"},
			[]string{"resultUrl"},
			[]string{"result_url"},
			[]string{"url"},
		), true
}

func extractVideoPostID(resp map[string]any) string {
	raw := mapAtAnyPath(resp,
		[]string{"streamingVideoGenerationResponse"},
		[]string{"streaming_video_generation_response"},
		[]string{"videoGenerationProgress"},
		[]string{"video_generation_progress"},
		[]string{"modelResponse", "streamingVideoGenerationResponse"},
		[]string{"modelResponse", "streaming_video_generation_response"},
	)
	if raw == nil {
		return ""
	}
	return stringAtAnyPath(raw,
		[]string{"videoPostId"},
		[]string{"video_post_id"},
	)
}

func extractAssetIDs(resp map[string]any) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(v any) {
		s := strings.TrimSpace(fmt.Sprint(v))
		if s == "" || s == "<nil>" {
			return
		}
		if _, exists := seen[s]; exists {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, item := range x {
				lk := strings.ToLower(strings.TrimSpace(k))
				switch lk {
				case "assetid", "asset_id":
					add(item)
				case "fileattachments", "file_attachments":
					if arr, ok := item.([]any); ok {
						for _, one := range arr {
							add(one)
						}
						continue
					}
					add(item)
				default:
					walk(item)
				}
			}
		case []any:
			for _, item := range x {
				walk(item)
			}
		}
	}
	walk(resp)
	return out
}

func assetURLFromAssetID(assetID string) string {
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(assetID), "http://") || strings.HasPrefix(strings.ToLower(assetID), "https://") {
		return assetID
	}
	if strings.Contains(assetID, "/") {
		return defaultAssetsBaseURL + "/" + strings.TrimLeft(assetID, "/")
	}
	return defaultAssetsBaseURL + "/" + assetID + "/content"
}

// assetURLs resolves each asset ID to a concrete URL, skipping empty results.
func assetURLs(assetIDs []string) []string {
	out := make([]string, 0, len(assetIDs))
	for _, id := range assetIDs {
		if u := assetURLFromAssetID(id); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// firstAssetURL returns the first resolvable media URL in an upstream response.
func firstAssetURL(resp map[string]any) string {
	return firstNonEmpty(assetURLs(extractAssetIDs(resp))...)
}

func appendImageResultURLs(urls []string, resp map[string]any) []string {
	added := make(map[string]struct{}, len(urls)+8)
	for _, u := range urls {
		added[u] = struct{}{}
	}
	addURL := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" {
			return
		}
		if _, exists := added[u]; exists {
			return
		}
		added[u] = struct{}{}
		urls = append(urls, u)
	}
	addURLs := func(items []string) {
		for _, u := range items {
			addURL(u)
		}
	}
	if mr := extractUpstreamModelResponse(resp); mr != nil {
		addURLs(extractImageURLs(mr))
		addURLs(assetURLs(extractAssetIDs(mr)))
	}
	addURLs(extractImageURLs(resp))
	addURLs(assetURLs(extractAssetIDs(resp)))
	return urls
}

func extractUpstreamModelResponse(resp map[string]any) map[string]any {
	return mapAtAnyPath(resp,
		[]string{"modelResponse"},
		[]string{"model_response"},
		[]string{"userResponse"},
		[]string{"user_response"},
		[]string{"messageResponse"},
		[]string{"message_response"},
		[]string{"output"},
	)
}

func extractUpstreamFingerprint(resp map[string]any, modelResp map[string]any) string {
	if fp := stringAtAnyPath(resp,
		[]string{"llmInfo", "modelHash"},
		[]string{"llm_info", "modelHash"},
		[]string{"llm_info", "model_hash"},
		[]string{"metadata", "llm_info", "modelHash"},
		[]string{"metadata", "llm_info", "model_hash"},
	); fp != "" {
		return fp
	}
	return stringAtAnyPath(modelResp,
		[]string{"metadata", "llm_info", "modelHash"},
		[]string{"metadata", "llm_info", "model_hash"},
		[]string{"metadata", "llmInfo", "modelHash"},
		[]string{"llmInfo", "modelHash"},
	)
}

func extractUpstreamResponseID(resp map[string]any, modelResp map[string]any) string {
	if rid := stringAtAnyPath(resp,
		[]string{"responseId"},
		[]string{"response_id"},
		[]string{"id"},
		[]string{"messageId"},
		[]string{"message_id"},
	); rid != "" {
		return rid
	}
	return stringAtAnyPath(modelResp,
		[]string{"responseId"},
		[]string{"response_id"},
		[]string{"id"},
		[]string{"messageId"},
		[]string{"message_id"},
	)
}

func extractUpstreamMessage(modelResp map[string]any) string {
	if modelResp == nil {
		return ""
	}
	if msg := stringAtAnyPath(modelResp,
		[]string{"message"},
		[]string{"content"},
		[]string{"text"},
		[]string{"outputText"},
		[]string{"output_text"},
	); msg != "" {
		return msg
	}
	if raw := mapAtAnyPath(modelResp, []string{"message"}, []string{"content"}, []string{"text"}); raw != nil {
		return stringAtAnyPath(raw,
			[]string{"text"},
			[]string{"content"},
			[]string{"value"},
			[]string{"body"},
		)
	}
	return ""
}

func extractUpstreamTokenDelta(resp map[string]any, modelResp map[string]any) string {
	if token := rawStringAtAnyPath(resp,
		[]string{"token"},
		[]string{"delta"},
		[]string{"text"},
		[]string{"contentDelta"},
		[]string{"content_delta"},
		[]string{"messageDelta"},
		[]string{"message_delta"},
	); token != "" {
		return token
	}
	return rawStringAtAnyPath(modelResp,
		[]string{"token"},
		[]string{"delta"},
		[]string{"textDelta"},
		[]string{"text_delta"},
	)
}
