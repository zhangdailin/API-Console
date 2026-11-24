package web

import (
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/httpclient"
)

const (
	maxChatDocumentBytes   = 16 << 20
	maxChatAttachmentBytes = 64 << 20
	maxChatMediaBytes      = 60 << 20
)

// Canonicalize user documents before the shared validator.
func normalizeChatDocuments(messages []ChatMessage) error {
	return chatwire.MapUserFileParts(messages, func(_, _ int, block map[string]any) (map[string]any, error) {
		source := block
		if nested, ok := block["file"].(map[string]any); ok {
			source = nested
		}
		id, _ := source["file_id"].(string)
		if topID, _ := block["file_id"].(string); strings.TrimSpace(id) != "" || strings.TrimSpace(topID) != "" {
			return nil, fmt.Errorf("Grok Web file_id is unsupported; use file_data or file_url")
		}
		data, _ := source["file_data"].(string)
		url, _ := source["file_url"].(string)
		data, url = strings.TrimSpace(data), strings.TrimSpace(url)
		if (data == "") == (url == "") {
			return nil, fmt.Errorf("provide exactly one of file_data or file_url")
		}
		if url != "" {
			if !isRemoteURL(url) {
				return nil, fmt.Errorf("file_url must be an HTTP or HTTPS URL")
			}
			data = url
		}
		name, _ := source["filename"].(string)
		return map[string]any{"type": "file", "file": map[string]any{"file_data": data, "filename": name}}, nil
	})
}

type preparedChatAttachment struct {
	document   bool
	name, mime string
	data       []byte
}

// Decode and check the complete batch before any upload. The decoded bytes are
// request-local; account switches may upload them again without fetching URLs again.
func (h *Handler) prepareChatAttachments(ctx context.Context, inputs []AttachmentInput) ([]preparedChatAttachment, error) {
	if len(inputs) > maxChatAttachments {
		return nil, fmt.Errorf("at most %d attachments are supported per request", maxChatAttachments)
	}
	seen := make(map[AttachmentInput]struct{}, len(inputs))
	out := make([]preparedChatAttachment, 0, len(inputs))
	remaining, documents := maxChatAttachmentBytes, maxChatDocumentBytes
	for _, input := range inputs {
		input.Data = strings.TrimSpace(input.Data)
		input.Filename = cleanChatAttachmentName(input.Filename)
		if _, ok := seen[input]; ok {
			continue
		}
		limit := min(remaining, maxChatMediaBytes)
		document := input.Type == "file"
		if document {
			limit = min(remaining, documents)
		}
		attachment, err := h.loadChatAttachment(ctx, input, limit, document)
		if err != nil {
			return nil, err
		}
		seen[input] = struct{}{}
		remaining -= len(attachment.data)
		if document {
			documents -= len(attachment.data)
		}
		out = append(out, attachment)
	}
	return out, nil
}

func (h *Handler) loadChatAttachment(ctx context.Context, input AttachmentInput, limit int, document bool) (preparedChatAttachment, error) {
	if limit <= 0 {
		return preparedChatAttachment{}, fmt.Errorf("chat attachments exceed the total size limit")
	}
	if err := ctx.Err(); err != nil {
		return preparedChatAttachment{}, err
	}
	name := cleanChatAttachmentName(input.Filename)
	var data []byte
	var fallback, mediaType string
	var err error
	if isRemoteURL(input.Data) {
		proxy := http.ProxyFromEnvironment
		if cfg := h.configSnapshot(); cfg != nil {
			proxy = httpclient.ProxyFuncFromConfig(cfg)
		}
		accept := "*/*"
		if !document {
			accept = "image/*, video/*;q=0.9, */*;q=0.8"
		}
		data, mediaType, err = fetchRemoteAttachment(ctx, input.Data, 30*time.Second, proxy, int64(limit), accept)
		if err != nil {
			if ctx.Err() != nil {
				return preparedChatAttachment{}, ctx.Err()
			}
			return preparedChatAttachment{}, fmt.Errorf("could not fetch the public attachment URL within the size limit")
		}
		if u, err := url.Parse(input.Data); err == nil && name == "" {
			name = cleanChatAttachmentName(u.Path)
		}
	} else {
		var encoded string
		fallback, encoded, mediaType, err = parseDataURI(input.Data)
		if err != nil {
			if document {
				return preparedChatAttachment{}, fmt.Errorf("file_data must be a Base64 data URI")
			}
			fallback, encoded, mediaType = "file.bin", input.Data, "application/octet-stream"
		}
		if base64.StdEncoding.DecodedLen(len(encoded)) > limit+2 {
			return preparedChatAttachment{}, fmt.Errorf("chat attachments exceed the size limit")
		}
		data, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(encoded)
		}
		if err != nil {
			return preparedChatAttachment{}, fmt.Errorf("attachment must contain valid Base64 data")
		}
	}
	if len(data) == 0 || len(data) > limit {
		return preparedChatAttachment{}, fmt.Errorf("attachment is empty or exceeds the size limit")
	}
	if document {
		name, mediaType, err = chatDocumentFormat(name, mediaType, data)
		if err != nil {
			return preparedChatAttachment{}, err
		}
	}
	if name == "" {
		name = cleanChatAttachmentName(fallback)
		if name == "" {
			name = "file.bin"
		}
	}
	return preparedChatAttachment{document: document, name: name, mime: mediaType, data: data}, nil
}

func cleanChatAttachmentName(name string) string {
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	if name == "." || name == ".." || name == "/" || len(name) > 255 {
		return ""
	}
	return name
}

func chatDocumentFormat(name, declared string, data []byte) (string, string, error) {
	mediaType, _, _ := mime.ParseMediaType(declared)
	ext := strings.ToLower(path.Ext(name))
	detected, _, _ := mime.ParseMediaType(http.DetectContentType(data))
	hasPDFHeader := strings.HasPrefix(string(data[:min(len(data), 5)]), "%PDF-")
	if hasPDFHeader {
		mediaType = "application/pdf"
		if ext != ".pdf" {
			name = "file.pdf"
		}
	} else if mediaType == "application/pdf" || ext == ".pdf" {
		return "", "", fmt.Errorf("attachment is not a valid PDF")
	}
	if mediaType == "" || mediaType == "application/octet-stream" || mediaType == "application/zip" {
		if hint := chatDocumentMIME(ext); hint != "" {
			mediaType = hint
		} else if mediaType != "application/zip" || detected != "application/octet-stream" {
			mediaType = detected
		}
	}
	return name, mediaType, nil
}

func chatDocumentMIME(ext string) string {
	switch ext {
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".doc":
		return "application/msword"
	case ".xls":
		return "application/vnd.ms-excel"
	case ".ppt":
		return "application/vnd.ms-powerpoint"
	case ".md", ".markdown":
		return "text/markdown"
	default:
		value, _, _ := mime.ParseMediaType(mime.TypeByExtension(ext))
		return value
	}
}
