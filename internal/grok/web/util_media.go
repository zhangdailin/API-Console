package web

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"encoding/json"
)

func extractImageURLs(value interface{}) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(raw string) {
		s := normalizeGrokAssetURL(raw)
		if s == "" {
			return
		}
		if _, exists := seen[s]; exists {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	var walk func(interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, item := range x {
				lk := strings.ToLower(k)
				if lk == "jsondata" || lk == "cardattachmentsjson" {
					walk(parseGrokJSONData(item))
					continue
				}
				if lk == "generatedimageurls" || lk == "imageurls" || lk == "image_urls" || lk == "imageurl" || lk == "image_url" {
					switch vv := item.(type) {
					case []interface{}:
						for _, one := range vv {
							if s, ok := one.(string); ok && s != "" {
								add(s)
							}
						}
					case string:
						add(vv)
					}
					continue
				}
				walk(item)
			}
		case []interface{}:
			for _, item := range x {
				walk(item)
			}
		case string:
			if parsed := parseGrokJSONText(x); parsed != nil {
				walk(parsed)
			}
		}
	}
	walk(value)
	return out
}

// extractRenderableImageLinks is a broad fallback for Grok tool/card payloads.
// Some Grok responses include image card/tool metadata where URLs aren't under the known keys.
// We conservatively collect http(s) links that look like images and point to Grok-related hosts.

func collectAssetLikeStrings(value interface{}, limit int) []string {
	out := make([]string, 0, 32)
	seen := map[string]struct{}{}
	walkJSONStrings(value, func(s string) {
		if limit > 0 && len(out) >= limit {
			return
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		ls := strings.ToLower(s)
		// Common patterns when no direct URL is provided.
		looksAsset := strings.Contains(ls, "assets") || strings.Contains(ls, "grok") || strings.Contains(ls, ".jpg") || strings.Contains(ls, ".png") || strings.Contains(ls, ".webp")
		looksPath := strings.HasPrefix(s, "/") && (strings.Contains(ls, "image") || strings.Contains(ls, "asset") || strings.Contains(ls, "."))
		if looksAsset || looksPath {
			if _, ok := seen[s]; !ok {
				seen[s] = struct{}{}
				out = append(out, s)
			}
		}
	})
	return out
}

func stripToolAndRenderMarkup(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	// Convert xai tool cards into readable text.
	text = reToolUsageCardBlock.ReplaceAllStringFunc(text, func(raw string) string {
		line := extractToolUsageCardText(raw)
		if line == "" {
			return ""
		}
		return "\n" + line + "\n"
	})
	// Drop incomplete tool cards.
	text = reToolUsageCardIncomplete.ReplaceAllString(text, "")
	// Remove grok render tags (allow optional leading '<')
	text = reGrokRenderBlock.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

func extractToolUsageCardText(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	rolloutID := strings.TrimSpace(extractToolUsageTagValue(raw, "rolloutId"))
	name := extractToolUsageTagValue(raw, "xai:tool_name")
	argsRaw := extractToolUsageTagValue(raw, "xai:tool_args")

	var payload struct {
		Query            string `json:"query"`
		Q                string `json:"q"`
		ImageDescription string `json:"image_description"`
		Description      string `json:"description"`
		Message          string `json:"message"`
	}
	if argsRaw != "" {
		_ = json.Unmarshal([]byte(argsRaw), &payload)
	}

	label := strings.TrimSpace(name)
	text := strings.TrimSpace(argsRaw)
	prefix := ""
	if rolloutID != "" {
		prefix = "[" + rolloutID + "]"
	}
	switch label {
	case "web_search":
		label = prefix + "[WebSearch]"
		if s := firstNonEmpty(payload.Query, payload.Q); s != "" {
			text = s
		}
	case "search_images":
		label = prefix + "[SearchImage]"
		if s := firstNonEmpty(payload.ImageDescription, payload.Description, payload.Query); s != "" {
			text = s
		}
	case "chatroom_send":
		label = prefix + "[AgentThink]"
		if s := payload.Message; s != "" {
			text = s
		}
	default:
		if label != "" && prefix != "" {
			label = prefix + label
		}
	}

	switch {
	case label != "" && text != "":
		return strings.TrimSpace(label + " " + text)
	case label != "":
		return label
	case text != "":
		return text
	default:
		// Fallback: strip tags and return plain text if any.
		return strings.TrimSpace(stripAngleTags(raw))
	}
}

func extractToolUsageTagValue(raw, tag string) string {
	if raw == "" || tag == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	openTag := "<" + strings.ToLower(tag) + ">"
	closeTag := "</" + strings.ToLower(tag) + ">"
	start := strings.Index(lower, openTag)
	if start < 0 {
		return ""
	}
	start += len(openTag)
	if start >= len(raw) {
		return ""
	}
	endRel := strings.Index(lower[start:], closeTag)
	if endRel < 0 {
		return ""
	}
	val := strings.TrimSpace(raw[start : start+endRel])
	if val == "" {
		return ""
	}
	trimmed := strings.TrimSpace(val)
	lowerTrimmed := strings.ToLower(trimmed)
	if strings.HasPrefix(lowerTrimmed, "<![cdata[") && strings.HasSuffix(lowerTrimmed, "]]>") && len(trimmed) >= len("<![CDATA[]]>") {
		trimmed = strings.TrimSpace(trimmed[len("<![CDATA[") : len(trimmed)-len("]]>")])
	}
	return strings.TrimSpace(trimmed)
}

func stripAngleTags(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '<':
			inTag = true
		case '>':
			if inTag {
				inTag = false
			} else {
				b.WriteByte(ch)
			}
		default:
			if !inTag {
				b.WriteByte(ch)
			}
		}
	}
	return b.String()
}

func extractRenderableImageLinks(value interface{}) []string {
	seen := map[string]struct{}{}
	var out []string

	isLikelyImageURL := func(s string) bool {
		s = strings.TrimSpace(s)
		ls := strings.ToLower(s)
		if !strings.HasPrefix(ls, "http://") && !strings.HasPrefix(ls, "https://") {
			return false
		}
		// Common image extensions or Grok CDN patterns.
		if strings.Contains(ls, "assets.grok.com") {
			return true
		}
		for _, ext := range renderableImageExtensions {
			if strings.Contains(ls, ext) {
				return true
			}
		}
		return false
	}

	walkJSONStrings(value, func(s string) {
		// Some fields may contain multiple URLs; split on whitespace.
		for _, part := range strings.Fields(s) {
			p := strings.Trim(part, "\"'()[]{}<>,")
			if isLikelyImageURL(p) {
				if _, ok := seen[p]; !ok {
					seen[p] = struct{}{}
					out = append(out, p)
				}
			}
		}
	})

	// Prefer higher-quality originals over tiny thumbnails.
	score := func(u string) int {
		lu := strings.ToLower(u)
		// Google tbn thumbnails are almost always low-res.
		if strings.Contains(lu, "encrypted-tbn0.gstatic.com") {
			return 0
		}
		if strings.Contains(lu, "thumbnail") || strings.Contains(lu, "thumb") {
			return 10
		}
		// Prefer URLs that look like direct image files.
		if strings.Contains(lu, ".jpg") || strings.Contains(lu, ".jpeg") || strings.Contains(lu, ".png") || strings.Contains(lu, ".webp") || strings.Contains(lu, ".gif") {
			return 100
		}
		return 50
	}
	sort.SliceStable(out, func(i, j int) bool {
		si := score(out[i])
		sj := score(out[j])
		if si == sj {
			return out[i] < out[j]
		}
		return si > sj
	})

	return out
}

func resolveAspectRatio(size string) string {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "512x512":
		return "1:1"
	case "1024x576", "1536x864":
		return "16:9"
	case "576x1024", "864x1536":
		return "9:16"
	case "512x768", "768x1024":
		return "2:3"
	case "768x512", "1024x768":
		return "3:2"
	}
	if ratio, err := normalizeImageAspectRatio("", size); err == nil {
		return ratio
	}
	return "2:3"
}

func resolveVideoSize(sizeOrRatio string) (aspectRatio string, resolutionName string, ok bool) {
	s := strings.ToLower(strings.TrimSpace(sizeOrRatio))
	if s == "" {
		s = "720x1280"
	}
	switch s {
	case "720x1280", "1024x1792", "9:16":
		return "9:16", "720p", true
	case "1280x720", "1792x1024", "16:9":
		return "16:9", "720p", true
	case "1024x1024", "1:1":
		return "1:1", "720p", true
	case "2:3":
		return "2:3", "720p", true
	case "3:2":
		return "3:2", "720p", true
	default:
		return "", "", false
	}
}

func canonicalVideoSize(sizeOrRatio string) string {
	s := strings.ToLower(strings.TrimSpace(sizeOrRatio))
	switch s {
	case "", "720x1280", "9:16":
		return "720x1280"
	case "1280x720", "16:9":
		return "1280x720"
	case "1024x1024", "1:1":
		return "1024x1024"
	case "1024x1792":
		return "1024x1792"
	case "1792x1024":
		return "1792x1024"
	case "2:3":
		return "1024x1792"
	case "3:2":
		return "1792x1024"
	default:
		return s
	}
}

func validateVideoConfig(cfg *VideoConfig) (*VideoConfig, error) {
	if cfg == nil {
		cfg = &VideoConfig{}
	}
	cfg.Normalize()

	ar := strings.TrimSpace(cfg.AspectRatio)
	if ar == "" && strings.TrimSpace(cfg.Size) != "" {
		var defaultResolution string
		var ok bool
		ar, defaultResolution, ok = resolveVideoSize(cfg.Size)
		if !ok {
			return nil, fmt.Errorf("size must be one of [720x1280 1280x720 1024x1024 1024x1792 1792x1024]")
		}
		if strings.TrimSpace(cfg.ResolutionName) == "" {
			cfg.ResolutionName = defaultResolution
		}
	}
	if ar == "" {
		ar = "720x1280"
	}
	mapped, ok := videoAspectRatioMap[ar]
	if !ok {
		return nil, fmt.Errorf("aspect_ratio must be one of [1280x720 720x1280 1792x1024 1024x1792 1024x1024 16:9 9:16 4:3 3:4 3:2 2:3 1:1]")
	}
	cfg.AspectRatio = mapped

	if cfg.VideoLength < 1 || cfg.VideoLength > 15 {
		return nil, fmt.Errorf("video_length must be between 1 and 15 seconds")
	}
	resolution := strings.TrimSpace(cfg.ResolutionName)
	if resolution == "" {
		resolution = "720p"
	}
	if resolution != "480p" && resolution != "720p" {
		return nil, fmt.Errorf("resolution_name must be one of ['480p', '720p']")
	}
	cfg.ResolutionName = resolution

	preset := strings.TrimSpace(cfg.Preset)
	switch preset {
	case "fun", "normal", "spicy", "custom":
	default:
		return nil, fmt.Errorf("preset must be one of ['fun', 'normal', 'spicy', 'custom']")
	}
	cfg.Preset = preset
	if strings.TrimSpace(cfg.Size) == "" {
		cfg.Size = canonicalVideoSize(ar)
	} else {
		cfg.Size = canonicalVideoSize(cfg.Size)
	}
	return cfg, nil
}

// validateVideoConfigForModel extends the legacy Web validator with the one
// extra resolution exposed by the official 1.5 media routes. Reference-image
// generation remains capped at 720p by both Console and Build.
func validateVideoConfigForModel(cfg *VideoConfig, spec ModelSpec, referenceCount int) (*VideoConfig, error) {
	if cfg == nil || !strings.EqualFold(strings.TrimSpace(cfg.ResolutionName), "1080p") {
		return validateVideoConfig(cfg)
	}
	if strings.TrimSpace(spec.UpstreamModel) != "grok-imagine-video-1.5" {
		return nil, fmt.Errorf("resolution_name 1080p is not available on the Grok Web plane")
	}
	if referenceCount > 0 {
		return nil, fmt.Errorf("reference image video generation supports at most 720p")
	}
	cloned := *cfg
	cloned.ResolutionName = "720p"
	validated, err := validateVideoConfig(&cloned)
	if err != nil {
		return nil, err
	}
	validated.ResolutionName = "1080p"
	return validated, nil
}

func videoSegmentLengths(seconds int) ([]int, error) {
	if seconds < 1 || seconds > 15 {
		return nil, fmt.Errorf("video_length must be between 1 and 15 seconds")
	}
	return []int{seconds}, nil
}

func videoExtensionStartTime(seconds int) float64 {
	return math.Round((float64(seconds)+1.0/24.0)*1e6) / 1e6
}

func normalizeImageSize(size string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(size))
	if s == "" {
		return "1024x1024", nil
	}
	switch s {
	case "1280x720", "720x1280", "1792x1024", "1024x1792", "1024x1024":
		return s, nil
	default:
		return "", fmt.Errorf("size must be one of 1280x720/720x1280/1792x1024/1024x1792/1024x1024")
	}
}

// validImageAspectRatio is grok2api's transport-layer check: only a real ratio
// string (or auto) is accepted here. The provider layer below still maps a pixel
// size to its ratio, which is what grok2api does too — but a caller cannot reach
// it through aspect_ratio, so `aspect_ratio=1280x720` is rejected exactly as the
// reference rejects it.
func validImageAspectRatio(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3",
		"2:1", "1:2", "19.5:9", "9:19.5", "20:9", "9:20":
		return true
	default:
		return false
	}
}

// validImageEditSize is grok2api's accepted `size` set for image edits.
func validImageEditSize(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "auto", "1024x1024", "1024x1536", "1536x1024":
		return true
	default:
		return false
	}
}

// normalizeImageResolution applies grok2api's resolution rule: empty defaults to
// 1k, and anything other than 1k/2k is a parameter error. The Web plane used to
// accept an unsupported resolution and silently render at 1k.
func normalizeImageResolution(value string) (string, error) {
	resolution := strings.ToLower(strings.TrimSpace(value))
	if resolution == "" {
		return "1k", nil
	}
	if resolution != "1k" && resolution != "2k" {
		return "", fmt.Errorf("resolution must be 1k or 2k")
	}
	return resolution, nil
}

func normalizeImageAspectRatio(aspectRatio, size string) (string, error) {
	values := map[string]string{
		"auto": "auto", "1:1": "1:1", "16:9": "16:9", "9:16": "9:16",
		"4:3": "4:3", "3:4": "3:4", "3:2": "3:2", "2:3": "2:3",
		"2:1": "2:1", "1:2": "1:2", "19.5:9": "19.5:9", "9:19.5": "9:19.5",
		"20:9": "20:9", "9:20": "9:20",
		"1280x720": "16:9", "720x1280": "9:16", "1792x1024": "3:2",
		"1536x1024": "3:2", "1024x1792": "2:3", "1024x1536": "2:3",
		"1024x1024": "1:1",
	}
	value := strings.ToLower(strings.TrimSpace(aspectRatio))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(size))
	}
	if value == "" {
		return "auto", nil
	}
	if resolved := values[value]; resolved != "" {
		return resolved, nil
	}
	return "", fmt.Errorf("aspect_ratio is not supported")
}

func normalizeImageEditSize(size string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(size))
	if s == "" {
		return "1024x1024", nil
	}
	switch s {
	case "auto", "1024x1024", "1024x1536", "1536x1024":
		return s, nil
	default:
		return "", fmt.Errorf("image edit size must be auto/1024x1024/1024x1536/1536x1024")
	}
}

func extractImageProgress(resp map[string]interface{}) (index int, progress int, ok bool) {
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

func imageChunkValuesFromAttachment(attachment map[string]interface{}) []map[string]interface{} {
	if attachment == nil {
		return nil
	}
	var values []map[string]interface{}
	appendChunkContainers := func(v interface{}) {
		m, ok := parseGrokJSONData(v).(map[string]interface{})
		if !ok || len(m) == 0 {
			return
		}
		for _, key := range []string{"image_chunk", "imageChunk", "image", "media"} {
			if chunk, ok := m[key].(map[string]interface{}); ok && len(chunk) > 0 {
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

func appChatImageChunkValues(resp map[string]interface{}) []map[string]interface{} {
	if resp == nil {
		return nil
	}
	var values []map[string]interface{}
	appendIfMap := func(v interface{}) {
		if m, ok := v.(map[string]interface{}); ok && len(m) > 0 {
			values = append(values, m)
		}
	}
	response := resp
	if nested := mapAtAnyPath(resp, []string{"result", "response"}); nested != nil {
		response = nested
	}
	if mr := extractUpstreamModelResponse(response); mr != nil {
		response = mr
	}
	for _, key := range []string{"streamingImageGenerationResponse", "imageGenerationResponse", "image_chunk", "imageChunk"} {
		appendIfMap(response[key])
	}
	if attachment, ok := response["cardAttachment"].(map[string]interface{}); ok {
		values = append(values, imageChunkValuesFromAttachment(attachment)...)
	}
	if metadata, ok := response["finalMetadata"].(map[string]interface{}); ok {
		for _, key := range []string{"image_chunk", "imageChunk", "streamingImageGenerationResponse", "imageGenerationResponse"} {
			appendIfMap(metadata[key])
		}
	}
	return values
}

func isModeratedImageChunk(chunk map[string]interface{}) bool {
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

func extractAppChatImageURLs(resp map[string]interface{}) []string {
	var urls []string
	seen := map[string]struct{}{}
	addURL := func(u string) {
		u = strings.TrimSpace(normalizeGrokAssetURL(u))
		if u == "" {
			return
		}
		if _, ok := seen[u]; ok {
			return
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}
	for _, chunk := range appChatImageChunkValues(resp) {
		progress := interfaceToInt(chunk["progress"])
		if progress == 0 {
			progress = interfaceToInt(chunk["percentage"])
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
		firstChunkString := func(keys ...string) string {
			for _, key := range keys {
				raw := strings.TrimSpace(fmt.Sprint(chunk[key]))
				if raw != "" && raw != "<nil>" {
					return raw
				}
			}
			return ""
		}
		assetID := firstChunkString("assetId", "asset_id", "fileId", "file_id")
		userID := firstChunkString("userId", "user_id")
		if assetID != "" && assetID != "<nil>" {
			if userID != "" && userID != "<nil>" {
				addURL("users/" + strings.Trim(userID, "/") + "/" + strings.Trim(assetID, "/") + "/content")
			} else if strings.Contains(assetID, "/") {
				addURL(assetID)
			}
		}
	}
	return urls
}

func appChatImageNoImageDiagnostics(resp map[string]interface{}) []string {
	if resp == nil {
		return nil
	}
	response := resp
	if nested := mapAtAnyPath(resp, []string{"result", "response"}); nested != nil {
		response = nested
	}
	var out []string
	if msg := stringAtAnyPath(response,
		[]string{"userResponse", "message"},
		[]string{"modelResponse", "message"},
		[]string{"message"},
		[]string{"error", "message"},
	); msg != "" {
		out = append(out, "message="+truncateDiagnosticText(msg, 220))
	}
	for _, path := range [][]string{
		{"userResponse", "streamErrors"},
		{"modelResponse", "streamErrors"},
		{"streamErrors"},
		{"errors"},
	} {
		if v := valueAtPath(response, path...); v != nil {
			if s := diagnosticValueSummary(v); s != "" {
				out = append(out, strings.Join(path, ".")+"="+s)
			}
		}
	}
	return out
}

func isAppChatImageLimitResponse(resp map[string]interface{}) bool {
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
		if strings.Contains(lower, "image generation limit") || strings.Contains(lower, "try again later") {
			return true
		}
	}
	return false
}

func extractVideoProgress(resp map[string]interface{}) (progress int, videoURL string, ok bool) {
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

func extractVideoPostID(resp map[string]interface{}) string {
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

func extractAssetIDs(resp map[string]interface{}) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(v interface{}) {
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
	var walk func(interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, item := range x {
				lk := strings.ToLower(strings.TrimSpace(k))
				switch lk {
				case "assetid", "asset_id":
					add(item)
				case "fileattachments", "file_attachments":
					if arr, ok := item.([]interface{}); ok {
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
		case []interface{}:
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
func firstAssetURL(resp map[string]interface{}) string {
	return firstNonEmpty(assetURLs(extractAssetIDs(resp))...)
}

func appendImageResultURLs(urls []string, resp map[string]interface{}) []string {
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

func imageDebugShape(resp map[string]interface{}) string {
	if resp == nil {
		return "nil"
	}
	var parts []string
	var walk func(interface{}, string, int)
	walk = func(v interface{}, prefix string, depth int) {
		if depth > 4 || len(parts) >= 40 {
			return
		}
		switch x := v.(type) {
		case map[string]interface{}:
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
		case []interface{}:
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

func extractUpstreamModelResponse(resp map[string]interface{}) map[string]interface{} {
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

func extractUpstreamFingerprint(resp map[string]interface{}, modelResp map[string]interface{}) string {
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

func extractUpstreamResponseID(resp map[string]interface{}, modelResp map[string]interface{}) string {
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

func extractUpstreamMessage(modelResp map[string]interface{}) string {
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

func extractUpstreamTokenDelta(resp map[string]interface{}, modelResp map[string]interface{}) string {
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
