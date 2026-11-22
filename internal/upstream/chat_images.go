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
	if strings.HasPrefix(strings.ToLower(value), "data:") {
		header, data, ok := strings.Cut(value[5:], ",")
		mime, base64Header := strings.CutSuffix(strings.ToLower(header), ";base64")
		if !ok || !base64Header || !b.takeInlineImage(mime, data) {
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

func (b *ChatImageBudget) takeInlineImage(mime, data string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return false
	}
	remaining := maxInlineImageSize - b.inlineBytes
	if len(data) == 0 || len(data) > base64.StdEncoding.EncodedLen(remaining) {
		return false
	}
	decoded := base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))
	var header [512]byte
	n, err := io.ReadFull(decoded, header[:])
	if err != nil && err != io.ErrUnexpectedEOF {
		return false
	}
	if http.DetectContentType(header[:n]) != mime {
		return false
	}
	rest, err := io.Copy(io.Discard, decoded)
	if size := n + int(rest); err == nil && size <= remaining {
		b.inlineBytes += size
		return true
	}
	return false
}
