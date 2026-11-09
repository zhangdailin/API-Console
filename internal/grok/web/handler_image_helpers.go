package web

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"net/http"
	"net/url"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/util"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func normalizeImageResponseFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "b64_json", "base64":
		return "b64_json"
	}
	return "url"
}

func imageResponseField(format string) string {
	if normalizeImageResponseFormat(format) == "b64_json" {
		return "b64_json"
	}
	return "url"
}

// imageMimeTypeForValue reports the media type of an image response entry.
// grok2api always states it; a client that trusts the declared type instead of
// sniffing the bytes needs the field to be present.
func imageMimeTypeForValue(field, value string) string {
	trimmed := strings.TrimSpace(value)
	if field == "b64_json" {
		if strings.HasPrefix(strings.ToLower(trimmed), "data:image/") {
			rest := trimmed[len("data:"):]
			if idx := strings.Index(rest, ";"); idx > 0 {
				return strings.ToLower(rest[:idx])
			}
		}
		return "image/" + imageOutputFormatFromBase64(trimmed)
	}
	if parsed, err := url.Parse(trimmed); err == nil {
		switch strings.ToLower(path.Ext(parsed.Path)) {
		case ".jpg", ".jpeg":
			return "image/jpeg"
		case ".webp":
			return "image/webp"
		case ".gif":
			return "image/gif"
		case ".png":
			return "image/png"
		}
	}
	return "image/png"
}

func imageOutputFormatFromBase64(value string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(raw) == 0 {
		return "png"
	}
	switch http.DetectContentType(raw) {
	case "image/jpeg":
		return "jpeg"
	case "image/webp":
		return "webp"
	default:
		return "png"
	}
}

func mediaExtFromMime(mediaType, mimeType, rawURL string) string {
	m := strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	switch m {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	}
	trim := strings.TrimSpace(rawURL)
	if idx := strings.Index(trim, "?"); idx >= 0 {
		trim = trim[:idx]
	}
	ext := strings.ToLower(strings.TrimSpace(filepath.Ext(trim)))
	if ext != "" && len(ext) <= 10 {
		return ext
	}
	if strings.EqualFold(mediaType, "video") {
		return ".mp4"
	}
	return ".jpg"
}

func imageDimsFromBytes(data []byte) (int, int) {
	if len(data) == 0 {
		return 0, 0
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

func isLikelyRasterImageBytes(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return true
	}
	if len(data) >= 8 &&
		data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G' &&
		data[4] == '\r' && data[5] == '\n' && data[6] == 0x1a && data[7] == '\n' {
		return true
	}
	if len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a") {
		return true
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return true
	}
	return false
}

func (h *Handler) cacheMediaURL(ctx context.Context, token, rawURL, mediaType string) (string, error) {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType != "video" {
		mediaType = "image"
	}
	trimURL := strings.TrimSpace(rawURL)
	lurl := strings.ToLower(trimURL)
	// Never cache known low-res thumbnail hosts; they lead to blurry results.
	if mediaType == "image" && strings.Contains(lurl, "encrypted-tbn0.gstatic.com") {
		return "", fmt.Errorf("skip thumbnail url")
	}
	// If the client can't reach Grok/X assets (common in some regions), caching through this server
	// is required for images to display at all.
	forceCache := mediaType == "image" && mustCacheImageURL(lurl)

	data, mimeType, err := h.webClient().downloadAsset(ctx, token, rawURL)
	if err != nil {
		return "", err
	}
	// Heuristic: avoid caching tiny/low-res images (often thumbnails/previews).
	if mediaType == "image" {
		if !isRasterImageMime(mimeType) {
			return "", fmt.Errorf("unsupported image mime type: %s", strings.TrimSpace(mimeType))
		}
		w, hgt := imageDimsFromBytes(data)
		if w <= 0 || hgt <= 0 {
			if !forceCache || !isLikelyRasterImageBytes(data) {
				return "", fmt.Errorf("unsupported image data")
			}
		}
		// For Grok/X image assets, caching is required for display (clients may not reach the CDN).
		if forceCache {
			// Always cache (even previews). We already avoid emitting -part-0 when full exists.
		} else {
			if (w > 0 && hgt > 0 && (w < 900 || hgt < 900)) || len(data) < 60*1024 {
				slog.Debug("skip caching low-res image", "url", trimURL, "bytes", len(data), "w", w, "h", hgt)
				return "", fmt.Errorf("skip low-res image")
			}
		}
	}
	return h.cacheMediaBytes(rawURL, mediaType, data, mimeType)
}

func isRasterImageMime(mimeType string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0])) {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func mustCacheImageURL(rawURL string) bool {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return false
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "/grok/v1/files/image/") || strings.HasPrefix(lower, "/v1/files/image/") {
		return false
	}
	if isLikelyImageAssetPath(raw) {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "assets.grok.com" || host == "grok.com" || strings.HasSuffix(host, ".grok.com") ||
		host == "x.ai" || strings.HasSuffix(host, ".x.ai") {
		return true
	}
	return false
}

func (h *Handler) imageOutputValue(ctx context.Context, token, url, format string) (string, error) {
	if normalizeImageResponseFormat(format) == "url" {
		trim := strings.TrimSpace(url)
		var cacheErr error
		// Stable contract: prefer full over -part-0. If we only got a preview URL,
		// try the full variant first.
		if strings.Contains(trim, "-part-0/") {
			full := strings.ReplaceAll(trim, "-part-0/", "/")
			if name, err := h.cacheMediaURL(ctx, token, full, "image"); err == nil && name != "" {
				return "/grok/v1/files/image/" + name, nil
			} else if err != nil {
				cacheErr = err
			}
		}
		if name, err := h.cacheMediaURL(ctx, token, trim, "image"); err == nil && name != "" {
			return "/grok/v1/files/image/" + name, nil
		} else if err != nil {
			cacheErr = err
		}
		// grok2api always lands the asset locally and answers with its own
		// address; handing the caller an upstream CDN URL (or a low-res
		// thumbnail that was skipped) is what the reference never does.
		if cacheErr != nil {
			return "", fmt.Errorf("cache grok image locally: %w", cacheErr)
		}
		return "", fmt.Errorf("cache grok image locally failed")
	}
	raw, _, err := h.webClient().downloadAsset(ctx, token, url)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// writeImageResults writes the non-streaming image generation/edit response.
// With strict=true a conversion failure aborts with 502; otherwise the failing
// entry falls back to the raw URL (url format) or an empty value (b64_json).
func (h *Handler) writeImageResults(w http.ResponseWriter, ctx context.Context, token, prompt string, urls []string, format, publicBase string, strict bool) {
	_ = strict // kept for callers: every conversion failure is fatal now.
	field := imageResponseField(format)
	data := make([]map[string]any, 0, len(urls))
	for _, u := range urls {
		val, err := h.imageOutputValue(ctx, token, u, format)
		if err != nil {
			// No pass-through: either the asset is cached and served from this
			// gateway, or the request fails. strict only decides the error shape.
			slog.Warn("grok image convert failed", "url", u, "error", err)
			writeGrokUpstreamError(w, err)
			return
		}
		if field == "url" {
			if publicBase == "" {
				httpserver.WriteErrorCode(w, http.StatusInternalServerError, "image_url_base_missing", "image URL base is not configured")
				return
			}
			if strings.HasPrefix(val, "/") {
				val = publicBase + val
			}
		}
		data = append(data, map[string]any{
			field: val,
			// Both fields are always present in grok2api's response: an empty
			// string (not null) for the unused revised prompt, and an explicit
			// media type.
			"revised_prompt": "",
			"mime_type":      imageMimeTypeForValue(field, val),
		})
	}
	util.WriteJSON(w, map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
		"usage":   buildImageUsagePayload(prompt, len(data)),
	})
}

// collectImageChatURLs runs the chat image edit/generation loop (n images from
// ceil(n/2) calls) and returns the normalized URLs. On failure it writes the
// error response and returns ok=false.
// collectImageChatURLs runs image edit calls until n edited images are in hand.
// One grok.com edit call produces one finished image (grok2api serves edits
// with n=1 only); the other images its stream carries are the progressive
// previews of that one image. Asking for two therefore takes two calls.
func (h *Handler) collectImageChatURLs(ctx context.Context, w http.ResponseWriter, sess *chatAccountSession, rawPayload *map[string]any, rebuildPayload func(string) (map[string]any, error), n int) ([]string, bool) {
	callsNeeded := max(n, 1)
	var urls []string
	excluded := make([]int64, 0, 4)
	for i, switches := 0, 0; i < callsNeeded && len(uniqueStrings(urls)) < n; i++ {
		resp, err := h.doChatWithAutoSwitchRebuild(ctx, sess, rawPayload, rebuildPayload)
		if err != nil {
			writeGrokUpstreamError(w, err)
			return nil, false
		}
		h.syncGrokQuota(sess.acc, resp.Header)
		// The edit stream echoes the uploaded inputs; they are not results.
		inputs := imageEditInputAssets(*rawPayload)
		limitHit := false
		var callURLs []string
		err = parseUpstreamLines(resp.Body, func(line map[string]any) error {
			if isAppChatImageLimitResponse(line) {
				limitHit = true
			}
			callURLs = appendImageEditResultURLs(callURLs, line)
			return nil
		})
		resp.Body.Close()
		if err != nil {
			writeGrokUpstreamError(w, err)
			return nil, false
		}
		callURLs = withoutInputAssets(callURLs, inputs)
		if len(callURLs) == 0 && limitHit {
			// This account's media allowance is spent: park it and try the
			// request on another account before giving up.
			h.noteMediaExhausted(ctx, sess.acc, mediaImageEdit)
			if sess.acc != nil && sess.acc.ID != 0 {
				excluded = append(excluded, sess.acc.ID)
			}
			if switches >= 3 || rebuildPayload == nil {
				httpserver.WriteErrorCode(w, http.StatusTooManyRequests, "media_quota_exhausted", h.mediaUnavailableMessage(ctx, mediaImageEdit))
				return nil, false
			}
			sess.Close()
			editSpec, _ := ResolveModel("grok-imagine-image-edit")
			next, switchErr := h.openChatAccountSessionForModelExcluding(ctx, excluded, editSpec)
			if switchErr != nil {
				writeGrokNoAccountError(w, switchErr)
				return nil, false
			}
			sess.acc, sess.token, sess.poolCandidates, sess.release = next.acc, next.token, next.poolCandidates, next.release
			rebuilt, rebuildErr := rebuildPayload(sess.token)
			if rebuildErr != nil {
				writeGrokUpstreamError(w, rebuildErr)
				return nil, false
			}
			*rawPayload = rebuilt
			switches++
			i--
			continue
		}
		urls = append(urls, callURLs...)
	}
	urls = normalizeGeneratedImageURLs(urls, n)
	if len(urls) == 0 {
		httpserver.WriteError(w, http.StatusBadGateway, "no image generated")
		return nil, false
	}
	return urls, true
}

// appendImageEditResultURLs adds the finished images of one edit stream line.
// A streamingImageGenerationResponse frame below 100% carries a progressive,
// still blurry preview of the result, and a moderated frame an image grok.com
// withheld; neither is a result.
func appendImageEditResultURLs(urls []string, line map[string]any) []string {
	if imageEditPreviewFrame(line) {
		return urls
	}
	return appendImageResultURLs(urls, line)
}

// imageEditPreviewFrame reports a progressive or moderated image frame.
func imageEditPreviewFrame(line map[string]any) bool {
	frame := mapAtAnyPath(line,
		[]string{"streamingImageGenerationResponse"},
		[]string{"streaming_image_generation_response"},
		[]string{"modelResponse", "streamingImageGenerationResponse"},
	)
	if frame == nil {
		return false
	}
	if moderated, _ := frame["moderated"].(bool); moderated {
		return true
	}
	if final, _ := frame["isFinal"].(bool); final {
		return false
	}
	raw, ok := frame["progress"]
	if !ok {
		return false
	}
	return interfaceToInt(raw) < 100
}

// imageEditInputAssets returns the asset IDs an edit request uploaded.
func imageEditInputAssets(payload map[string]any) []string {
	media, _ := payload["mediaGenInput"].(map[string]any)
	edit, _ := media["imageToImage"].(map[string]any)
	var out []string
	switch assets := edit["inputAssets"].(type) {
	case []string:
		out = append(out, assets...)
	case []any:
		for _, value := range assets {
			if id, ok := value.(string); ok {
				out = append(out, id)
			}
		}
	}
	return out
}

// withoutInputAssets drops the URLs that point at one of the request's own
// uploaded inputs.
func withoutInputAssets(urls, inputs []string) []string {
	if len(inputs) == 0 {
		return urls
	}
	out := urls[:0]
	for _, value := range urls {
		echoed := false
		for _, id := range inputs {
			if id = strings.TrimSpace(id); id != "" && strings.Contains(value, id) {
				echoed = true
				break
			}
		}
		if !echoed {
			out = append(out, value)
		}
	}
	return out
}

func (h *Handler) cacheMediaBytes(rawURL, mediaType string, data []byte, mimeType string) (string, error) {
	return writeMediaCacheFile(mediaType, data, func(mediaType string) string {
		sum := sha1.Sum([]byte(strings.TrimSpace(rawURL)))
		return hex.EncodeToString(sum[:]) + mediaExtFromMime(mediaType, mimeType, rawURL)
	})
}

// writeMediaCacheFile stores data in the media cache under the name nameFor
// derives from the normalized media type ("image" or "video"). The name is
// content-addressed by the caller, so an existing non-empty file is reused,
// and the temp-file rename keeps a partially written file from being served.
func writeMediaCacheFile(kind string, data []byte, nameFor func(mediaType string) string) (string, error) {
	mediaType := strings.ToLower(strings.TrimSpace(kind))
	if mediaType != "video" {
		mediaType = "image"
	}
	if len(data) == 0 {
		return "", fmt.Errorf("empty media data")
	}
	dir := filepath.Join(cacheBaseDir, mediaType)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := nameFor(mediaType)
	fullPath := filepath.Join(dir, name)
	if info, statErr := os.Stat(fullPath); statErr == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return name, nil
	}
	tmp := fullPath + ".tmp-" + randomHex(4)
	if writeErr := os.WriteFile(tmp, data, 0o644); writeErr != nil {
		_ = os.Remove(tmp)
		return "", writeErr
	}
	if renameErr := os.Rename(tmp, fullPath); renameErr != nil {
		_ = os.Remove(tmp)
		return "", renameErr
	}
	return name, nil
}

// imageSizeFromBase64 reports the pixel size of a base64 image as "WxH", or
// "auto" when it cannot be determined. A streamed image event used to always
// say "auto"; a client that sizes its placeholder from the event had to guess.
func imageSizeFromBase64(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "auto"
	}
	if idx := strings.Index(trimmed, ","); strings.HasPrefix(strings.ToLower(trimmed), "data:") && idx > 0 {
		trimmed = trimmed[idx+1:]
	}
	raw, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil || len(raw) == 0 {
		return "auto"
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return "auto"
	}
	return strconv.Itoa(cfg.Width) + "x" + strconv.Itoa(cfg.Height)
}

// imageEventSize picks the reported size for an image event: the real pixel
// size for base64 payloads, and "auto" for a URL (whose bytes are not fetched
// here).
func imageEventSize(field, value string) string {
	if field != "b64_json" {
		return "auto"
	}
	return imageSizeFromBase64(value)
}

// nonNilStrings returns values, or an empty non-nil slice for none.
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
