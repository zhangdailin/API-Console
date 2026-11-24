package web

import (
	"fmt"
	"net/url"
	"orchids-api/internal/responses"
	"sort"
	"strings"
)

func extractImageProgress(resp map[string]any) (index int, progress int, ok bool) {
	raw := mapAtAnyPath(resp, "streamingImageGenerationResponse", "streaming_image_generation_response", "imageGenerationProgress", "image_generation_progress", "modelResponse.streamingImageGenerationResponse", "modelResponse.streaming_image_generation_response")
	if raw == nil {
		return 0, 0, false
	}
	return intAtAnyPath(raw, "imageIndex", "image_index", "index"),
		intAtAnyPath(raw, "progress", "percent", "percentage", "completedPercentage"), true
}

func imageChunkValuesFromAttachment(attachment map[string]any) []map[string]any {
	if attachment == nil {
		return nil
	}
	var values []map[string]any
	appendChunkContainers := func(v any) {
		m, ok := parseGrokJSONData(v).(map[string]any)
		if !ok || len(m) == 0 {
			return
		}
		for _, key := range []string{"image_chunk", "imageChunk", "image", "media"} {
			if chunk, ok := m[key].(map[string]any); ok && len(chunk) > 0 {
				values = append(values, chunk)
			}
		}
		values = append(values, m)
	}
	appendChunkContainers(attachment["jsonData"])
	appendChunkContainers(attachment["json"])
	appendChunkContainers(attachment["data"])
	appendChunkContainers(attachment["metadata"])
	return values
}

func appChatImageChunkValues(resp map[string]any) []map[string]any {
	if resp == nil {
		return nil
	}
	var values []map[string]any
	appendIfMap := func(v any) {
		if m, ok := v.(map[string]any); ok && len(m) > 0 {
			values = append(values, m)
		}
	}
	response := resp
	if nested := mapAtAnyPath(resp, "result.response"); nested != nil {
		response = nested
	}
	if mr := extractUpstreamModelResponse(response); mr != nil {
		response = mr
	}
	for _, key := range []string{"streamingImageGenerationResponse", "imageGenerationResponse", "image_chunk", "imageChunk"} {
		appendIfMap(response[key])
	}
	if attachment, ok := response["cardAttachment"].(map[string]any); ok {
		values = append(values, imageChunkValuesFromAttachment(attachment)...)
	}
	if metadata, ok := response["finalMetadata"].(map[string]any); ok {
		for _, key := range []string{"image_chunk", "imageChunk", "streamingImageGenerationResponse", "imageGenerationResponse"} {
			appendIfMap(metadata[key])
		}
	}
	return values
}

func isModeratedImageChunk(chunk map[string]any) bool {
	if chunk == nil {
		return false
	}
	for _, key := range []string{"moderated", "isModerated", "blocked", "isBlocked", "contentFiltered"} {
		if v, ok := chunk[key].(bool); ok && v {
			return true
		}
	}
	status := strings.ToLower(strings.TrimSpace(fmt.Sprint(chunk["status"])))
	return status == "blocked" || status == "moderated"
}

func extractAppChatImageURLs(resp map[string]any) []string {
	var urls []string
	seen := map[string]struct{}{}
	addURL := func(u string) {
		u = strings.TrimSpace(normalizeGrokAssetURL(u))
		// Only images grok.com drew are results. Cards also carry searched
		// web pictures and link previews; returning those as "generated"
		// images hands the caller pictures unrelated to the prompt.
		if u == "" || !isGrokHostedAssetURL(u) {
			return
		}
		if _, ok := seen[u]; ok {
			return
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}
	for _, chunk := range appChatImageChunkValues(resp) {
		progress := responses.InterfaceToInt(chunk["progress"])
		if progress == 0 {
			progress = responses.InterfaceToInt(chunk["percentage"])
		}
		if progress > 0 && progress < 100 {
			continue
		}
		if isModeratedImageChunk(chunk) {
			continue
		}
		for _, key := range []string{"imageUrl", "url", "mediaUrl", "generatedImageUrl", "assetUrl"} {
			if raw := strings.TrimSpace(fmt.Sprint(chunk[key])); raw != "" && raw != "<nil>" {
				addURL(raw)
			}
		}
		assetID := stringAtAnyPath(chunk, "assetId", "asset_id", "fileId", "file_id")
		userID := stringAtAnyPath(chunk, "userId", "user_id")
		if assetID != "" {
			if userID != "" {
				addURL("users/" + strings.Trim(userID, "/") + "/" + strings.Trim(assetID, "/") + "/content")
			} else if strings.Contains(assetID, "/") {
				addURL(assetID)
			}
		}
	}
	return urls
}

// isGrokHostedAssetURL reports whether u is served by grok.com itself
// (assets.grok.com or another grok.com host), as generated images are.
func isGrokHostedAssetURL(u string) bool {
	parsed, err := url.Parse(strings.TrimSpace(u))
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "grok.com" || strings.HasSuffix(host, ".grok.com")
}

func appChatImageNoImageDiagnostics(resp map[string]any) []string {
	if resp == nil {
		return nil
	}
	response := resp
	if nested := mapAtAnyPath(resp, "result.response"); nested != nil {
		response = nested
	}
	var out []string
	if msg := stringAtAnyPath(response, "userResponse.message", "modelResponse.message", "message", "error.message"); msg != "" {
		out = append(out, "message="+truncateDiagnosticText(msg, 220))
	}
	for _, path := range []string{
		"userResponse.streamErrors",
		"modelResponse.streamErrors",
		"streamErrors",
		"errors",
	} {
		if v := valueAtPath(response, path); v != nil {
			if s := diagnosticValueSummary(v); s != "" {
				out = append(out, path+"="+s)
			}
		}
	}
	return out
}

func isAppChatImageLimitResponse(resp map[string]any) bool {
	if resp == nil {
		return false
	}
	response := resp
	if nested := mapAtAnyPath(resp, "result.response"); nested != nil {
		response = nested
	}
	for _, path := range []string{
		"error.renderToolRateLimited",
		"userResponse.streamErrors",
		"modelResponse.streamErrors",
		"streamErrors",
		"errors",
	} {
		if strings.Contains(strings.ToLower(diagnosticValueSummary(valueAtPath(response, path))), "rendertoolratelimited") {
			return true
		}
	}
	for _, msg := range []string{
		stringAtAnyPath(response, "error.message"),
		stringAtAnyPath(response, "modelResponse.message"),
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
	raw := videoProgressObject(resp)
	if raw == nil {
		return 0, "", false
	}
	return intAtAnyPath(raw, "progress", "percent", "percentage", "completedPercentage"),
		stringAtAnyPath(raw, "videoUrl", "videoURL", "video_url", "resultUrl", "result_url", "url"), true
}

func extractVideoPostID(resp map[string]any) string {
	raw := videoProgressObject(resp)
	if raw == nil {
		return ""
	}
	return stringAtAnyPath(raw, "videoPostId", "video_post_id")
}

func videoProgressObject(resp map[string]any) map[string]any {
	return mapAtAnyPath(resp, "streamingVideoGenerationResponse", "streaming_video_generation_response", "videoGenerationProgress", "video_generation_progress", "modelResponse.streamingVideoGenerationResponse", "modelResponse.streaming_video_generation_response")
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

func imageDebugShape(resp map[string]any) string {
	if resp == nil {
		return "nil"
	}
	var parts []string
	var walk func(any, string, int)
	walk = func(v any, prefix string, depth int) {
		if depth > 4 || len(parts) >= 40 {
			return
		}
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if prefix != "" {
				parts = append(parts, prefix+"{"+strings.Join(keys, ",")+"}")
			} else {
				parts = append(parts, "{"+strings.Join(keys, ",")+"}")
			}
			for _, k := range keys {
				lk := strings.ToLower(k)
				if strings.Contains(lk, "image") ||
					strings.Contains(lk, "card") ||
					strings.Contains(lk, "error") ||
					strings.Contains(lk, "message") ||
					strings.Contains(lk, "moderation") ||
					strings.Contains(lk, "progress") ||
					strings.Contains(lk, "response") ||
					strings.Contains(lk, "result") {
					next := k
					if prefix != "" {
						next = prefix + "." + k
					}
					walk(x[k], next, depth+1)
				}
			}
		case []any:
			parts = append(parts, prefix+"[]")
			if len(x) > 0 {
				walk(x[0], prefix+"[]", depth+1)
			}
		case string:
			if prefix != "" {
				parts = append(parts, prefix+"=string")
			}
		case float64, bool, nil:
			if prefix != "" {
				parts = append(parts, prefix+"="+fmt.Sprintf("%T", x))
			}
		}
	}
	walk(resp, "", 0)
	return strings.Join(parts, " ")
}

func extractUpstreamModelResponse(resp map[string]any) map[string]any {
	return mapAtAnyPath(resp, "modelResponse", "model_response", "userResponse", "user_response", "messageResponse", "message_response", "output")
}

func extractUpstreamFingerprint(resp map[string]any, modelResp map[string]any) string {
	if fp := stringAtAnyPath(resp, "llmInfo.modelHash", "llm_info.modelHash", "llm_info.model_hash", "metadata.llm_info.modelHash", "metadata.llm_info.model_hash"); fp != "" {
		return fp
	}
	return stringAtAnyPath(modelResp, "metadata.llm_info.modelHash", "metadata.llm_info.model_hash", "metadata.llmInfo.modelHash", "llmInfo.modelHash")
}

func extractUpstreamResponseID(resp map[string]any, modelResp map[string]any) string {
	for _, object := range []map[string]any{resp, modelResp} {
		if id := stringAtAnyPath(object, "responseId", "response_id", "id", "messageId", "message_id"); id != "" {
			return id
		}
	}
	return ""
}

func extractUpstreamMessage(modelResp map[string]any) string {
	if modelResp == nil {
		return ""
	}
	if msg := stringAtAnyPath(modelResp, "message", "content", "text", "outputText", "output_text"); msg != "" {
		return msg
	}
	if raw := mapAtAnyPath(modelResp, "message", "content", "text"); raw != nil {
		return stringAtAnyPath(raw, "text", "content", "value", "body")
	}
	return ""
}

func extractUpstreamTokenDelta(resp map[string]any, modelResp map[string]any) string {
	if token := rawStringAtAnyPath(resp, "token", "delta", "text", "contentDelta", "content_delta", "messageDelta", "message_delta"); token != "" {
		return token
	}
	return rawStringAtAnyPath(modelResp, "token", "delta", "textDelta", "text_delta")
}
