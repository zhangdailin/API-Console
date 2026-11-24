package upstream

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"

	"orchids-api/internal/httpclient"
	"orchids-api/internal/prompt"
)

const (
	maxChatImages      = 8
	maxInlineImageSize = 16 << 20
)

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

func (m ChatMessage) MarshalJSON() ([]byte, error) {
	type plain ChatMessage
	if len(m.parts) == 0 {
		return json.Marshal(plain(m))
	}
	return json.Marshal(struct {
		plain
		Content []chatContentPart `json:"content"`
	}{plain(m), m.parts})
}

var errInvalidChatImage = errors.New("invalid chat image")

// ChatImageBudget limits images across a single chat request. The zero value
// starts a fresh budget.
type ChatImageBudget struct {
	count       int
	inlineBytes int
}

func (b *ChatImageBudget) imageURL(block prompt.ContentBlock) (string, bool) {
	if b.count >= maxChatImages {
		return "", false
	}
	value := strings.TrimSpace(block.URL)
	if value == "" && block.Source != nil {
		source := block.Source
		switch strings.ToLower(strings.TrimSpace(source.Type)) {
		case "url":
			value = strings.TrimSpace(source.URL)
		case "base64":
			mime := strings.ToLower(strings.TrimSpace(source.MediaType))
			if mime == "" {
				mime = "image/png"
			}
			data := strings.TrimSpace(source.Data)
			if !b.takeInlineImage(mime, data) {
				return "", false
			}
			b.count++
			return "data:" + mime + ";base64," + data, true
		}
	}
	if mime, data, isDataURL := SplitInlineImageURL(value); isDataURL {
		if !b.takeInlineImage(mime, data) {
			return "", false
		}
	} else {
		// Forward the reference for the upstream to handle. Check URL shape
		// and block clearly local hosts and IPs without dialing them.
		parsed, err := httpclient.ValidateRemoteFetchURL(value)
		if err != nil {
			return "", false
		}
		host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
		addr, ipErr := netip.ParseAddr(host)
		if (ipErr == nil && !httpclient.IsPublicAddr(addr)) || (ipErr != nil && !strings.Contains(host, ".")) ||
			strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
			return "", false
		}
	}
	b.count++
	return value, true
}

// SplitInlineImageURL splits a Base64 data URL without copying the payload.
// The boolean identifies the scheme even if the header is invalid.
func SplitInlineImageURL(value string) (mime, data string, isDataURL bool) {
	const prefix, suffix = "data:", ";base64"
	value = strings.TrimSpace(value)
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return "", "", false
	}
	header, data, ok := strings.Cut(value[len(prefix):], ",")
	end := len(header) - len(suffix)
	if !ok || end <= 0 || !strings.EqualFold(header[end:], suffix) {
		return "", "", true
	}
	return strings.ToLower(header[:end]), data, true
}

func (b *ChatImageBudget) takeInlineImage(mime, data string) bool {
	size, ok := InlineImageSize(mime, data, maxInlineImageSize-b.inlineBytes)
	if ok {
		b.inlineBytes += size
	}
	return ok
}

// InlineImageSize validates Base64 raster data within a decoded byte limit
// without retaining a copy of the decoded image.
func InlineImageSize(mime, data string, limit int) (int, bool) {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return 0, false
	}
	if limit <= 0 || len(data) == 0 || len(data) > base64.StdEncoding.EncodedLen(limit) {
		return 0, false
	}
	decoded := base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))
	var header [512]byte
	n, err := io.ReadFull(decoded, header[:])
	if err != nil && err != io.ErrUnexpectedEOF {
		return 0, false
	}
	if http.DetectContentType(header[:n]) != mime {
		return 0, false
	}
	rest, err := io.Copy(io.Discard, decoded)
	if size := n + int(rest); err == nil && size <= limit {
		return size, true
	}
	return 0, false
}
