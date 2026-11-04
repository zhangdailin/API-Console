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
