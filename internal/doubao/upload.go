package doubao

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"orchids-api/internal/httpclient"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

// uploadTenant and uploadScene are the values the chat input box sends for a
// user file attachment: tenant 5, scene 131 (MOA_USER_SKILLS).
const (
	uploadTenant = "5"
	// File attachments use the user-skill scene; images use the bot-chat scene.
	// The two scenes resolve to different object-store services, and an image
	// stored through the file scene is accepted by the IM gateway but never
	// reaches the vision path, which is why such a turn produced no answer.
	uploadFileScene  = "131"
	uploadImageScene = "5"
	uploadRegion     = "cn-north-1"
	uploadSvc        = "imagex"
)

// uploadTicket is one prepared object: where to PUT it and the pre-signed token
// the object store accepts for that path.
type uploadTicket struct {
	StoreURI string
	Auth     string
	Host     string
}

// prepareUpload obtains short-lived object-store credentials from the gateway.
//
// This is the step earlier reverse-engineering missed: ApplyImageUpload needs an
// AWS4-HMAC-SHA256 signature, and its secret is not hardcoded in the front end.
// The page first calls /alice/resource/prepare_upload, which returns a
// short-lived STS triple, and that makes the signature reproducible without a
// browser capture.
func (c *Client) prepareUpload(ctx context.Context, jar cookieJar, scene string, resourceType int) (http.Header, []byte, error) {
	body, _ := json.Marshal(map[string]any{
		"tenant_id":     uploadTenant,
		"scene_id":      scene,
		"resource_type": resourceType,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+"/alice/resource/prepare_upload", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header = c.headers(jar, false)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	if err := httpclient.DecodeContentEncoding(response); err != nil {
		return nil, nil, err
	}
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode >= 400 {
		return nil, nil, statusError(response.StatusCode, raw)
	}
	return response.Header, raw, nil
}

// objectStoreHTTP is the plain pooled client used for object-store uploads. The
// signed Authorization travels in a header, so the uTLS/browser fingerprint is
// not needed here and would break the HTTP/1.1 upload host.
func (c *Client) objectStoreHTTP() *http.Client {
	proxy := httpclient.ProxyFuncFromConfig(c.cfg)
	key := "doubao-tos|" + httpclient.GenerateProxyKeyFromConfig(c.cfg)
	return httpclient.GetSharedHTTPClientWithLimits(key, 0, proxy, false, c.cfg)
}

type uploadCredentials struct {
	ServiceID    string
	Host         string
	AccessKey    string
	SecretKey    string
	SessionToken string
}

func (c *Client) uploadCredentials(ctx context.Context, jar cookieJar, scene string, resourceType int) (uploadCredentials, error) {
	_, raw, err := c.prepareUpload(ctx, jar, scene, resourceType)
	if err != nil {
		return uploadCredentials{}, err
	}
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ServiceID  string `json:"service_id"`
			UploadHost string `json:"upload_host"`
			Token      struct {
				AccessKey    string `json:"access_key"`
				SecretKey    string `json:"secret_key"`
				SessionToken string `json:"session_token"`
			} `json:"upload_auth_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return uploadCredentials{}, fmt.Errorf("doubao upload credentials are not JSON")
	}
	if envelope.Code != 0 {
		return uploadCredentials{}, fmt.Errorf("doubao upload credentials refused: code=%d %s", envelope.Code, compactError(envelope.Msg))
	}
	creds := uploadCredentials{
		ServiceID:    envelope.Data.ServiceID,
		Host:         first(envelope.Data.UploadHost, "imagex.bytedanceapi.com"),
		AccessKey:    envelope.Data.Token.AccessKey,
		SecretKey:    envelope.Data.Token.SecretKey,
		SessionToken: envelope.Data.Token.SessionToken,
	}
	if creds.AccessKey == "" || creds.SecretKey == "" || creds.SessionToken == "" {
		return uploadCredentials{}, fmt.Errorf("doubao upload credentials are incomplete")
	}
	return creds, nil
}

// uploadAttachment stores data in Doubao's object store and returns the
// StoreUri the chat attachment block references.
func (c *Client) uploadAttachment(ctx context.Context, jar cookieJar, name string, data []byte, image bool) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("doubao attachment is empty")
	}
	scene, resourceType := uploadFileScene, 1
	if image {
		scene, resourceType = uploadImageScene, 2
	}
	creds, err := c.uploadCredentials(ctx, jar, scene, resourceType)
	if err != nil {
		return "", err
	}
	ext := strings.ToLower(path.Ext(name))
	if ext == "" {
		ext = ".bin"
	}
	if image && ext == ".jpeg" {
		ext = ".jpg"
	}
	ticket, err := c.applyUpload(ctx, creds, len(data), ext)
	if err != nil {
		return "", err
	}
	if err := c.putObject(ctx, ticket, data, mimeOf(ext)); err != nil {
		return "", err
	}
	return ticket.StoreURI, nil
}

// applyUpload signs the ApplyImageUpload request. The object store accepts only
// the date and the STS token in SignedHeaders, so those two are the only ones
// covered by the signature.
func (c *Client) applyUpload(ctx context.Context, creds uploadCredentials, size int, ext string) (uploadTicket, error) {
	query := url.Values{
		"Action":        {"ApplyImageUpload"},
		"Version":       {"2018-08-01"},
		"ServiceId":     {creds.ServiceID},
		"NeedFallback":  {"true"},
		"FileSize":      {fmt.Sprint(size)},
		"FileExtension": {ext},
	}
	encoded := query.Encode()
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	authorization := sigV4Authorization(creds, amzDate, http.MethodGet, "/", encoded)

	target := "https://" + creds.Host + "/?" + encoded
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return uploadTicket{}, err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-security-token", creds.SessionToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())
	response, err := c.http.Do(req)
	if err != nil {
		return uploadTicket{}, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode >= 400 {
		return uploadTicket{}, fmt.Errorf("doubao upload ticket refused: status=%d %s", response.StatusCode, compactError(string(raw)))
	}
	var envelope struct {
		Result struct {
			UploadAddress struct {
				StoreInfos []struct {
					StoreURI string `json:"StoreUri"`
					Auth     string `json:"Auth"`
				} `json:"StoreInfos"`
				UploadHosts []string `json:"UploadHosts"`
			} `json:"UploadAddress"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return uploadTicket{}, fmt.Errorf("doubao upload ticket is not JSON")
	}
	infos := envelope.Result.UploadAddress.StoreInfos
	if len(infos) == 0 || infos[0].StoreURI == "" || infos[0].Auth == "" {
		return uploadTicket{}, fmt.Errorf("doubao upload ticket is incomplete")
	}
	host := creds.Host
	if len(envelope.Result.UploadAddress.UploadHosts) > 0 && envelope.Result.UploadAddress.UploadHosts[0] != "" {
		host = envelope.Result.UploadAddress.UploadHosts[0]
	}
	return uploadTicket{StoreURI: infos[0].StoreURI, Auth: infos[0].Auth, Host: host}, nil
}

// putObject writes the bytes. The store requires Content-CRC32 in lowercase
// hex; a decimal value is accepted syntactically but fails the checksum.
func (c *Client) putObject(ctx context.Context, ticket uploadTicket, data []byte, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, objectURL(ticket), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", ticket.Auth)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-CRC32", fmt.Sprintf("%08x", crc32.ChecksumIEEE(data)))
	req.Header.Set("Origin", c.baseURL())
	req.Header.Set("Referer", c.baseURL()+"/")
	req.Header.Set("User-Agent", c.userAgent())
	// The object store negotiates HTTP/1.1 only; the browser-shaped client
	// insists on HTTP/2 for TLS and fails with an unexpected ALPN protocol.
	response, err := c.objectStoreHTTP().Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
	if response.StatusCode >= 400 {
		return fmt.Errorf("doubao upload failed: status=%d %s", response.StatusCode, compactError(string(raw)))
	}
	var envelope struct {
		Success int `json:"success"`
		Error   struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Success != 0 && envelope.Error.Code != 0 {
		return fmt.Errorf("doubao upload failed: code=%d", envelope.Error.Code)
	}
	return nil
}

// objectURL selects the write path the upload host expects. The two object
// stores the gateway can be handed use different routes: the image service
// (tos-hl-x.snssdk.com) exposes /upload/v1/<uri>, while the file host used for
// chat documents accepts /<uri>.
func objectURL(ticket uploadTicket) string {
	prefix := "/"
	if strings.Contains(ticket.Host, "snssdk.com") {
		prefix = "/upload/v1/"
	}
	return "https://" + ticket.Host + prefix + ticket.StoreURI
}

// sigV4Authorization reproduces the signature layout observed from the web
// client: Credential carries the date scope, and only x-amz-date and
// x-amz-security-token are signed.
func sigV4Authorization(creds uploadCredentials, amzDate, method, targetPath, rawQuery string) string {
	date := amzDate[:8]
	scope := date + "/" + uploadRegion + "/" + uploadSvc + "/aws4_request"
	signedHeaders := []string{"x-amz-date", "x-amz-security-token"}
	canonicalHeaders := "x-amz-date:" + amzDate + "\n" +
		"x-amz-security-token:" + creds.SessionToken + "\n"
	canonicalRequest := strings.Join([]string{
		method,
		targetPath,
		canonicalQuery(rawQuery),
		canonicalHeaders,
		strings.Join(signedHeaders, ";"),
		sha256Hex(nil),
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	key := hmacSHA256([]byte("AWS4"+creds.SecretKey), date)
	key = hmacSHA256(key, uploadRegion)
	key = hmacSHA256(key, uploadSvc)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))
	return fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKey, scope, strings.Join(signedHeaders, ";"), signature)
}

// canonicalQuery sorts the encoded pairs the way SigV4 requires.
func canonicalQuery(raw string) string {
	pairs := make([][2]string, 0, 8)
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		key, value, _ := strings.Cut(part, "=")
		pairs = append(pairs, [2]string{sigV4Encode(key), sigV4Encode(value)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	out := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, pair[0]+"="+pair[1])
	}
	return strings.Join(out, "&")
}

func sigV4Encode(value string) string {
	decoded, err := url.QueryUnescape(value)
	if err != nil {
		decoded = value
	}
	escaped := url.QueryEscape(decoded)
	return strings.NewReplacer("+", "%20", "%7E", "~").Replace(escaped)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// decodeBase64File accepts the encodings chat clients send for file_data.
func decodeBase64File(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "data:") {
		if index := strings.Index(value, ","); index >= 0 {
			value = value[index+1:]
		}
	}
	value, _, _ = strings.Cut(value, ",")
	return base64.StdEncoding.DecodeString(strings.TrimSpace(value))
}

// attachmentMIME maps the extensions the chat input accepts. The object store
// stores the type with the object, and the parser uses it to decide how to read
// the file.
var attachmentMIME = map[string]string{
	".txt": "text/plain", ".md": "text/markdown", ".json": "application/json",
	".csv": "text/csv", ".pdf": "application/pdf", ".png": "image/png",
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".zip": "application/zip",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

func mimeOf(ext string) string {
	if value, ok := attachmentMIME[strings.ToLower(ext)]; ok {
		return value
	}
	return "application/octet-stream"
}

// storeURI is one uploaded object referenced by a chat attachment block.
type storeURI struct {
	URI  string
	Name string
	Size int
	// Image marks an image attachment. The web client uses a different
	// attachment shape for images (type 2 with an image field) than for files
	// (type 3 with a file field), and the model only sees the image when the
	// image shape is used.
	Image bool
	// Width and Height describe images. The web client sends them with the
	// attachment, and the renderer uses them for the placeholder box.
	Width, Height int
	// Format is the image format without a dot, as the attachment reports it.
	Format string
}

// uploadRequestAttachments uploads every image/file block in the request and
// returns the references the chat body must carry. Attachments belong to the
// latest user turn, so only the last message is considered.
func (c *Client) uploadRequestAttachments(ctx context.Context, jar cookieJar, req upstream.UpstreamRequest) ([]storeURI, error) {
	var blocks []promptBlock
	for index := len(req.Messages) - 1; index >= 0; index-- {
		message := req.Messages[index]
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		for _, block := range message.Content.GetBlocks() {
			if data, name, ok := attachmentData(block); ok {
				blocks = append(blocks, promptBlock{data: data, name: name, image: isImageBlock(block)})
				continue
			}
			// A file block that only carries a URL is fetched here rather than
			// handed to the model as a bare link: the model's browsing path may
			// answer from the title alone, and a fetch makes the content
			// deterministic.
			if remote := remoteFileURL(block); remote != "" {
				text, err := c.fetchURLText(ctx, remote)
				if err != nil {
					return nil, fmt.Errorf("doubao could not read the attached url: %w", err)
				}
				blocks = append(blocks, promptBlock{data: []byte(text), name: fileNameFromURL(remote)})
			}
		}
		break
	}
	if len(blocks) == 0 {
		return nil, nil
	}
	out := make([]storeURI, 0, len(blocks))
	for _, block := range blocks {
		uri, err := c.uploadAttachment(ctx, jar, block.name, block.data, block.image)
		if err != nil {
			return nil, err
		}
		slog.Debug("doubao attachment uploaded", "name", block.name, "bytes", len(block.data), "image", block.image, "uri", uri)
		entry := storeURI{URI: uri, Name: block.name, Size: len(block.data), Image: block.image}
		if block.image {
			entry.Width, entry.Height, entry.Format = imageDimensions(block.data)
			if entry.Format == "" {
				entry.Format = strings.TrimPrefix(strings.ToLower(path.Ext(block.name)), ".")
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// imageDimensions reads the original size the attachment block reports. The
// vision path uses it to lay out the image; an unknown format falls back to the
// filename extension and zero dimensions.
func imageDimensions(data []byte) (int, int, string) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, ""
	}
	return config.Width, config.Height, format
}

type promptBlock struct {
	data  []byte
	name  string
	image bool
}

func isImageBlock(block prompt.ContentBlock) bool {
	switch block.Type {
	case "image", "image_url", "input_image":
		return true
	}
	return false
}

// attachmentData extracts the bytes a media block carries. Only inline data and
// data: URLs are accepted; a remote URL would make the gateway fetch arbitrary
// addresses on the caller's behalf.
func attachmentData(block prompt.ContentBlock) ([]byte, string, bool) {
	name := first(block.FileName, block.URL, "attachment.bin")
	switch block.Type {
	case "image", "image_url", "input_image":
		if block.Source != nil && block.Source.Data != "" {
			data, err := decodeBase64File(block.Source.Data)
			if err == nil && len(data) > 0 {
				return data, imageName(block.Source.MediaType), true
			}
		}
		if strings.HasPrefix(block.URL, "data:") {
			data, err := decodeBase64File(block.URL)
			if err == nil && len(data) > 0 {
				return data, "image.png", true
			}
		}
	case "file", "input_file":
		if block.FileData != "" {
			data, err := decodeBase64File(block.FileData)
			if err == nil && len(data) > 0 {
				return data, name, true
			}
		}
		if strings.HasPrefix(block.URL, "data:") {
			data, err := decodeBase64File(block.URL)
			if err == nil && len(data) > 0 {
				return data, name, true
			}
		}
	}
	return nil, "", false
}

// remoteFileURL reports the URL an attachment block points at, when it points
// at an http(s) resource instead of carrying inline bytes. The caller fetches it
// through the outbound guard and uploads the result, so a URL attachment behaves
// like an inline one.
func remoteFileURL(block prompt.ContentBlock) string {
	switch block.Type {
	case "file", "input_file":
		for _, candidate := range []string{block.URL, block.FileData} {
			if isHTTPURL(candidate) {
				return strings.TrimSpace(candidate)
			}
		}
	}
	return ""
}

func isHTTPURL(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// fileNameFromURL derives a filename from a URL path so the attachment keeps a
// useful extension for the upstream parser.
func fileNameFromURL(rawURL string) string {
	if parsed, err := url.Parse(strings.TrimSpace(rawURL)); err == nil {
		if base := path.Base(parsed.Path); base != "" && base != "." && base != "/" {
			return base
		}
	}
	return "attachment.txt"
}

func imageName(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/jpeg", "image/jpg":
		return "image.jpg"
	case "image/gif":
		return "image.gif"
	case "image/webp":
		return "image.webp"
	default:
		return "image.png"
	}
}

// attachmentMessages builds one attachment block per uploaded object. The web
// client sends them ahead of the text block in the same request.
func attachmentMessages(attachments []storeURI) []any {
	out := make([]any, 0, len(attachments))
	for _, attachment := range attachments {
		entry := map[string]any{
			"identifier":    randomUUID(),
			"review_state":  1,
			"progress":      100,
			"upload_status": 1,
		}
		if attachment.Image {
			// The captured image attachment uses type 1 and carries the original
			// dimensions under image_ori. It is also left at parse_state 0: an
			// image is not text-parsed, and claiming a finished parse made the
			// IM gateway accept the block while the vision path never ran.
			entry["type"] = 1
			entry["parse_state"] = 0
			entry["image"] = map[string]any{
				"name": attachment.Name,
				"uri":  attachment.URI,
				"image_ori": map[string]any{
					"url":         "",
					"width":       attachment.Width,
					"height":      attachment.Height,
					"format":      attachment.Format,
					"url_formats": map[string]any{},
				},
			}
			entry["src"] = ""
		} else {
			entry["parse_state"] = 1
			entry["type"] = 3
			entry["file"] = map[string]any{"name": attachment.Name, "uri": attachment.URI, "size": attachment.Size}
		}
		out = append(out, map[string]any{
			"local_message_id": randomUUID(), "message_status": 0,
			"content_block": []any{map[string]any{
				"block_type": 10052, "block_id": randomUUID(), "parent_id": "",
				"meta_info": []any{}, "extra_info": map[string]any{}, "append_fields": []any{},
				"content": map[string]any{"attachment_block": map[string]any{"attachments": []any{entry}}},
			}},
		})
	}
	return out
}
