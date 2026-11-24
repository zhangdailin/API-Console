package warp

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

const (
	maxWarpImages = 8
	// Warp may echo user context in a Base64-wrapped stream frame and persist
	// it for tool turns. Leave room for both encodings and the enclosing state.
	maxWarpInlineImageBytes = 512 << 10
)

type warpImage struct {
	mime   string
	base64 string
}

func rawImageBlock(block map[string]json.RawMessage, kind, role, path string) error {
	if role != "user" {
		return wireFieldError(path+".type", "images are supported only in user messages")
	}
	if cache, exists := block["cache_control"]; exists {
		if err := rawCacheControl(cache, path+".cache_control"); err != nil {
			return err
		}
	}
	switch kind {
	case "image":
		if !rawKeys(block, "type", "source", "cache_control") {
			return wireFieldError(path, "unsupported image field")
		}
		var source map[string]json.RawMessage
		var sourceType, mime, data string
		if json.Unmarshal(block["source"], &source) != nil || source == nil ||
			!rawKeys(source, "type", "media_type", "data") ||
			!rawString(source["type"], &sourceType) || sourceType != "base64" ||
			!rawString(source["media_type"], &mime) || !warpImageMIME(mime) ||
			!rawString(source["data"], &data) || data == "" {
			return wireFieldError(path+".source", "requires a Base64 PNG or JPEG source")
		}
	default:
		if !rawKeys(block, "type", "image_url", "detail", "cache_control") || !validImageDetail(block["detail"]) {
			return wireFieldError(path, "unsupported image URL field")
		}
		var value string
		if kind != "input_image" || !rawString(block["image_url"], &value) {
			var imageURL map[string]json.RawMessage
			if json.Unmarshal(block["image_url"], &imageURL) != nil || imageURL == nil ||
				!rawKeys(imageURL, "url", "detail") || !validImageDetail(imageURL["detail"]) ||
				!rawString(imageURL["url"], &value) {
				return wireFieldError(path+".image_url", "requires an inline Base64 image URL")
			}
		}
		if mime, data, _ := upstream.SplitInlineImageURL(value); !warpImageMIME(mime) || data == "" {
			return wireFieldError(path+".image_url", "requires an inline Base64 PNG or JPEG data URL")
		}
	}
	return nil
}

func validImageDetail(raw json.RawMessage) bool {
	var hint string
	return raw == nil || rawString(raw, &hint) && (hint == "auto" || hint == "low" || hint == "high")
}

func warpImageMIME(mime string) bool { return mime == "image/png" || mime == "image/jpeg" }

func normalizedWarpImage(block prompt.ContentBlock) (warpImage, bool) {
	if !validCacheControl(block.CacheControl) {
		return warpImage{}, false
	}
	expected := prompt.ContentBlock{Type: block.Type, CacheControl: block.CacheControl}
	var img warpImage
	switch block.Type {
	case "image":
		source := block.Source
		if source == nil || source.Type != "base64" || source.URL != "" {
			return warpImage{}, false
		}
		expected.Source = source
		img = warpImage{mime: source.MediaType, base64: source.Data}
	case "image_url", "input_image":
		expected.URL = block.URL
		img.mime, img.base64, _ = upstream.SplitInlineImageURL(block.URL)
	}
	return img, warpImageMIME(img.mime) && reflect.DeepEqual(expected, block)
}

// prepareImageContext keeps blobs out of the text transcript. The positional
// marker preserves attachment order, including when a turn contains images
// alone. Bound tool turns later select only images from the current user turn.
func prepareImageContext(messages []prompt.Message) ([]prompt.Message, []warpImage, int, error) {
	firstCurrent := len(messages) - 1
	for firstCurrent > 0 && messages[firstCurrent-1].Role == "user" {
		firstCurrent--
	}
	out := messages
	var images []warpImage
	var historical int
	remaining := maxWarpInlineImageBytes
	for i, message := range messages {
		if i == firstCurrent {
			historical = len(images)
		}
		var blocks []prompt.ContentBlock
		for j, block := range message.Content.Blocks {
			if block.Type != "image" && block.Type != "image_url" && block.Type != "input_image" {
				continue
			}
			img, ok := normalizedWarpImage(block)
			if !ok || message.Role != "user" || len(images) >= maxWarpImages {
				return nil, nil, 0, apperrors.NewLocalRequestError("warp_image")
			}
			size, ok := upstream.InlineImageSize(img.mime, img.base64, remaining)
			if !ok {
				return nil, nil, 0, apperrors.NewLocalRequestError("warp_image")
			}
			if strings.ContainsAny(img.base64, "\r\n") {
				img.base64 = strings.ReplaceAll(strings.ReplaceAll(img.base64, "\r", ""), "\n", "")
			}
			if len(images) == 0 {
				out = slices.Clone(messages)
			}
			if blocks == nil {
				blocks = slices.Clone(message.Content.Blocks)
				out[i].Content.Blocks = blocks
			}
			blocks[j] = prompt.ContentBlock{Type: "text", Text: "\n[Attached image]\n"}
			remaining -= size
			images = append(images, img)
		}
	}
	return out, images, historical, nil
}
