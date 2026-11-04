package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"orchids-api/internal/httpclient"
)

// Imagine edits take their inputs from the Imagine upload path, not from the
// chat attachment upload: an asset uploaded as a chat attachment is not an
// edit source, and the edit model then draws a fresh image instead of editing
// it. grok2api uploads edit inputs the same way.
const (
	imagineUploadPath       = "/http/upload-file-v2/direct"
	imagineSelfUploadSource = "IMAGINE_SELF_UPLOAD_FILE_SOURCE"
)

// uploadImagineInput uploads one edit input and returns its fileMetadataId.
func (c *Client) uploadImagineInput(ctx context.Context, token, filename, mimeType string, data []byte) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	name := strings.NewReplacer("\r", "", "\n", "", `"`, "").Replace(strings.TrimSpace(filename))
	if name == "" {
		name = "image.png"
	}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, name))
	header.Set("Content-Type", mimeType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := writer.WriteField("file_source", imagineSelfUploadSource); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	reqURL := c.baseURL() + imagineUploadPath
	headers := c.headersFor(ctx, token, http.MethodPost, reqURL)
	headers.Set("Content-Type", writer.FormDataContentType())
	headers.Set("Referer", c.baseURL()+"/imagine")
	headers.Del("x-xai-request-id")
	reqCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, reqURL, bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", err
	}
	req.Header = headers
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &grokUpstreamError{status: resp.StatusCode, body: string(raw), header: resp.Header}
	}
	var value struct {
		UploadID      string          `json:"uploadId"`
		TerminalError json.RawMessage `json:"terminalError"`
		FileMetadata  struct {
			ID     string `json:"fileMetadataId"`
			FileID string `json:"fileId"`
		} `json:"fileMetadata"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("imagine upload: invalid response")
	}
	if terminal := strings.TrimSpace(string(value.TerminalError)); terminal != "" && terminal != "null" && terminal != "{}" && terminal != "false" {
		return "", fmt.Errorf("imagine upload rejected by the upstream")
	}
	for _, id := range []string{value.FileMetadata.ID, value.FileMetadata.FileID, value.UploadID} {
		if id = strings.TrimSpace(id); id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("imagine upload returned no fileMetadataId")
}

// uploadImageEditInput resolves one edit input (a data URI or a remote URL)
// and uploads it through the Imagine upload path.
func (h *Handler) uploadImageEditInput(ctx context.Context, token, input string) (string, error) {
	data := strings.TrimSpace(input)
	if data == "" {
		return "", fmt.Errorf("empty image input")
	}
	if isRemoteURL(data) {
		proxyFunc := http.ProxyFromEnvironment
		if cfg := h.configSnapshot(); cfg != nil {
			proxyFunc = httpclient.ProxyFuncFromConfig(cfg)
		}
		fetched, err := fetchRemoteAsDataURI(data, 30*time.Second, proxyFunc)
		if err != nil {
			return "", err
		}
		data = fetched
	}
	filename, contentBase64, mimeType, err := parseDataURI(data)
	if err != nil {
		return "", fmt.Errorf("image input must be an image URL or a data URI")
	}
	raw, err := base64.StdEncoding.DecodeString(contentBase64)
	if err != nil {
		return "", fmt.Errorf("image input is not valid base64")
	}
	return h.webClient().uploadImagineInput(ctx, token, filename, mimeType, raw)
}
