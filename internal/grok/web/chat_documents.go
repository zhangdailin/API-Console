package web

import (
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode"

	"orchids-api/internal/httpclient"
)

const maxChatDocumentBytes = 16 << 20

// Canonicalize documents before the shared validator. Ignore empty text
// placeholders only in a user message that actually contains a document.
func normalizeChatDocuments(messages []ChatMessage) error {
	for i := range messages {
		msg := &messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			continue
		}
		parts, ok := msg.Content.([]any)
		if !ok {
			continue
		}
		hasDocument := false
		next := make([]any, 0, len(parts))
		for _, part := range parts {
			block, _ := part.(map[string]any)
			kind, _ := block["type"].(string)
			switch strings.ToLower(strings.TrimSpace(kind)) {
			case "file", "input_file":
				source := block
				if nested, ok := block["file"].(map[string]any); ok {
					source = nested
				}
				id, _ := source["file_id"].(string)
				if topID, _ := block["file_id"].(string); strings.TrimSpace(id) != "" || strings.TrimSpace(topID) != "" {
					return fmt.Errorf("Grok Web file_id is unsupported; use file_data or file_url")
				}
				data, _ := source["file_data"].(string)
				url, _ := source["file_url"].(string)
				data, url = strings.TrimSpace(data), strings.TrimSpace(url)
				if (data == "") == (url == "") {
					return fmt.Errorf("provide exactly one of file_data or file_url")
				}
				if url != "" {
					if !isRemoteURL(url) {
						return fmt.Errorf("file_url must be an HTTP or HTTPS URL")
					}
					data = url
				}
				name, _ := source["filename"].(string)
				part = map[string]any{"type": "file", "file": map[string]any{"file_data": data, "filename": name}}
				hasDocument = true
			}
			next = append(next, part)
		}
		if hasDocument {
			kept := next[:0]
			for _, part := range next {
				block, _ := part.(map[string]any)
				kind, _ := block["type"].(string)
				if value, ok := block["text"].(string); ok && strings.TrimSpace(value) == "" &&
					(strings.EqualFold(strings.TrimSpace(kind), "text") || strings.EqualFold(strings.TrimSpace(kind), "input_text")) {
					continue
				}
				kept = append(kept, part)
			}
			msg.Content = kept
		}
	}
	return nil
}

func (h *Handler) loadChatDocument(ctx context.Context, input AttachmentInput, limit int) (string, string, []byte, error) {
	source := input.Data
	if isRemoteURL(source) {
		proxy := http.ProxyFromEnvironment
		if cfg := h.configSnapshot(); cfg != nil {
			proxy = httpclient.ProxyFuncFromConfig(cfg)
		}
		var err error
		source, err = fetchRemoteAsDataURI(ctx, source, 30*time.Second, proxy, int64(limit))
		if err != nil {
			if ctx.Err() != nil {
				return "", "", nil, ctx.Err()
			}
			return "", "", nil, fmt.Errorf("could not fetch the public document URL within the attachment limits")
		}
	}
	fallback, encoded, mediaType, err := parseDataURI(source)
	if err != nil {
		return "", "", nil, fmt.Errorf("file_data must be a Base64 data URI")
	}
	if base64.StdEncoding.DecodedLen(len(encoded)) > limit+2 {
		return "", "", nil, fmt.Errorf("chat documents exceed the attachment size limit")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > limit {
		return "", "", nil, fmt.Errorf("chat document must contain valid Base64 data within the attachment size limit")
	}
	mediaType, _, err = mime.ParseMediaType(mediaType)
	if err != nil || !strings.Contains(mediaType, "/") {
		return "", "", nil, fmt.Errorf("invalid attachment media type")
	}
	name := path.Base(strings.ReplaceAll(strings.TrimSpace(input.Filename), "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	if name == "." || name == "/" || name == "" || len(name) > 255 {
		name = path.Base(fallback)
	}
	if strings.HasPrefix(string(data[:min(len(data), 5)]), "%PDF-") {
		mediaType = "application/pdf"
		if !strings.EqualFold(path.Ext(name), ".pdf") {
			name = "file.pdf"
		}
	}
	if mediaType == "application/pdf" && (len(data) < 5 || string(data[:5]) != "%PDF-") {
		return "", "", nil, fmt.Errorf("attachment is not a valid PDF")
	}
	return name, mediaType, data, nil
}
