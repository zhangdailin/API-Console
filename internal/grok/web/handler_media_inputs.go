package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/store"
)

const (
	mediaInputTTL             = 24 * time.Hour
	maxMediaInputBytes        = 20 << 20
	maxMediaInputRequestBytes = maxMediaInputBytes + (1 << 20)
	maxResolvedMediaBytes     = 32 << 20
	localMediaInputPrefix     = "orchids-media-input:"
)

var mediaInputQuotaMu sync.Mutex
var mediaInputReservedBytes int64

var (
	errAdminMediaTooLarge  = errors.New("media input exceeds 20 MiB")
	errAdminMediaBlocked   = errors.New("media input URL is not allowed")
	adminMediaInputFetcher = fetchAdminMediaInput
)

const maxAdminMediaImportURLBytes = 8192

func (h *Handler) HandleMediaInputs(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "media input store is not configured")
		return
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "multipart/form-data") {
		writeResponsesAPIError(w, http.StatusUnsupportedMediaType, "invalid_request", "media input upload requires multipart/form-data")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMediaInputRequestBytes)
	if err := r.ParseMultipartForm(maxMediaInputRequestBytes); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeResponsesAPIError(w, http.StatusRequestEntityTooLarge, "media_too_large", "media input exceeds 20 MiB")
			return
		}
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "invalid media input upload")
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "exactly one file is required")
		return
	}
	fileHeader := files[0]
	if fileHeader.Size <= 0 {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_media", "media input cannot be empty")
		return
	}
	if fileHeader.Size > maxMediaInputBytes {
		writeResponsesAPIError(w, http.StatusRequestEntityTooLarge, "media_too_large", "media input exceeds 20 MiB")
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_media", "failed to read media input")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxMediaInputBytes+1))
	if err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_media", "failed to read media input")
		return
	}
	if len(data) > maxMediaInputBytes {
		writeResponsesAPIError(w, http.StatusRequestEntityTooLarge, "media_too_large", "media input exceeds 20 MiB")
		return
	}
	kind, mimeType, err := detectMediaInput(data, fileHeader.Header.Get("Content-Type"))
	if err != nil {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_media", err.Error())
		return
	}
	if !reserveMediaInputBytes(int64(len(data))) {
		writeResponsesAPIError(w, http.StatusInsufficientStorage, "media_storage_full",
			"media input storage is full; retry after the expired inputs are reclaimed")
		return
	}
	reserved := true
	defer func() {
		if reserved {
			releaseMediaInputBytes(int64(len(data)))
		}
	}()
	id, err := newMediaInputID()
	if err != nil {
		writeResponsesAPIError(w, http.StatusInternalServerError, "internal_error", "failed to allocate media input")
		return
	}
	name, err := h.cacheMediaInputBytes(id, kind, data, mimeType)
	if err != nil {
		writeResponsesAPIError(w, http.StatusInternalServerError, "internal_error", "failed to cache media input")
		return
	}
	contentPath := filepath.Join(cacheBaseDir, kind, name)
	now := time.Now().UTC()
	input := &store.StoredMediaInput{
		ID: id, OwnerHash: videoRequestOwner(r), Kind: kind, MIMEType: mimeType,
		ContentPath: contentPath, SizeBytes: int64(len(data)), CreatedAt: now,
	}
	if err := h.lb.Store.SaveStoredMediaInput(r.Context(), input, mediaInputTTL); err != nil {
		_ = os.Remove(contentPath)
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "failed to persist media input")
		return
	}
	reserved = false
	writeJSONStatus(w, http.StatusCreated, map[string]interface{}{
		"file_id": id, "object": "file", "kind": kind, "mime_type": mimeType,
		"bytes": len(data), "created_at": now.Unix(), "expires_at": now.Add(mediaInputTTL).Format(time.RFC3339),
	})
}

// mediaInputFilePrefix namespaces uploaded media inputs inside the shared image
// and video cache directories. The background sweeper reclaims files that carry
// this prefix once their store record has expired; generated media never does,
// so a sweep can never delete an asset a client or a completed video job still
// points at.
const mediaInputFilePrefix = "input-"

// maxMediaInputTotalBytes caps the bytes held by live media inputs. grok2api
// refuses an upload past a configured total (507) rather than filling the disk
// and discovering it later.
const maxMediaInputTotalBytes = 2 << 30

// reserveMediaInputBytes serializes the usage check with concurrent uploads.
func reserveMediaInputBytes(size int64) bool {
	if size <= 0 {
		return false
	}
	mediaInputQuotaMu.Lock()
	defer mediaInputQuotaMu.Unlock()
	used, err := mediaInputUsageBytes()
	if err != nil || used+mediaInputReservedBytes > maxMediaInputTotalBytes-size {
		return false
	}
	mediaInputReservedBytes += size
	return true
}

func releaseMediaInputBytes(size int64) {
	mediaInputQuotaMu.Lock()
	mediaInputReservedBytes -= size
	if mediaInputReservedBytes < 0 {
		mediaInputReservedBytes = 0
	}
	mediaInputQuotaMu.Unlock()
}

// mediaInputUsageBytes sums the size of the namespaced media input files.
func mediaInputUsageBytes() (int64, error) {
	var total int64
	for _, kind := range []string{"image", "video"} {
		entries, err := os.ReadDir(filepath.Join(cacheBaseDir, kind))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return total, err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasPrefix(entry.Name(), mediaInputFilePrefix) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			total += info.Size()
		}
	}
	return total, nil
}

// cacheMediaInputBytes stores an uploaded media input under the namespaced name.
// The content hash keeps the write idempotent, and the atomic rename keeps a
// partially written file from ever being served.
func (h *Handler) cacheMediaInputBytes(id, kind string, data []byte, mimeType string) (string, error) {
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
	sum := sha1.Sum([]byte("input:" + strings.TrimSpace(id)))
	name := mediaInputFilePrefix + hex.EncodeToString(sum[:]) + mediaExtFromMime(mediaType, mimeType, "")
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

func (h *Handler) HandleMediaInputResource(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "media input store is not configured")
		return
	}
	id := mediaInputIDFromPath(r.URL.Path)
	if !validMediaInputID(id) {
		writeGrokError(w, http.StatusNotFound, "media input not found")
		return
	}
	owner := videoRequestOwner(r)
	input, err := h.loadMediaInput(r.Context(), id, owner)
	if err != nil {
		writeGrokError(w, http.StatusNotFound, "media input not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]interface{}{
			"file_id": input.ID, "object": "file", "kind": input.Kind, "mime_type": input.MIMEType,
			"bytes": input.SizeBytes, "created_at": input.CreatedAt.Unix(), "expires_at": input.ExpiresAt.Format(time.RFC3339),
		})
	case http.MethodDelete:
		if err := h.lb.Store.DeleteStoredMediaInput(r.Context(), id, owner); err != nil {
			writeGrokError(w, http.StatusNotFound, "media input not found")
			return
		}
		if validCachedMediaContentPath(input.ContentPath, input.Kind) {
			_ = os.Remove(input.ContentPath)
		}
		writeJSON(w, map[string]interface{}{"id": id, "object": "file", "deleted": true})
	default:
		writeGrokError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func newMediaInputID() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "input_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func validMediaInputID(id string) bool {
	encoded, ok := strings.CutPrefix(strings.TrimSpace(id), "input_")
	if !ok {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	return err == nil && len(raw) == 24
}

func mediaInputIDFromPath(path string) string {
	for _, prefix := range []string{"/grok/v1/media/inputs/", "/v1/media/inputs/"} {
		if strings.HasPrefix(path, prefix) {
			return strings.Trim(strings.TrimPrefix(path, prefix), "/")
		}
	}
	return ""
}

func detectMediaInput(data []byte, declared string) (string, string, error) {
	if len(data) == 0 {
		return "", "", fmt.Errorf("media input cannot be empty")
	}
	detected := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0]))
	switch detected {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return "image", detected, nil
	}
	if len(data) >= 12 && string(data[4:8]) == "ftyp" {
		declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
		if declared == "video/quicktime" {
			return "video", declared, nil
		}
		// An ISO base-media box by itself says nothing about the codec: HEIC,
		// AVIF and other still images share the "ftyp" signature. Only accept it
		// as video when the major brand (or the caller's own declaration) says
		// so — otherwise a photo would be forwarded to the upstream as video/mp4.
		brand := strings.ToLower(strings.TrimSpace(string(data[8:12])))
		if strings.HasPrefix(declared, "video/") || videoISOBrand(brand) {
			if strings.HasPrefix(declared, "video/") {
				return "video", declared, nil
			}
			return "video", "video/mp4", nil
		}
		return "", "", fmt.Errorf("only valid jpeg, png, webp, gif, mp4, webm, or quicktime media is supported")
	}
	if len(data) >= 4 && bytes.Equal(data[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}) {
		return "video", "video/webm", nil
	}
	return "", "", fmt.Errorf("only valid jpeg, png, webp, gif, mp4, webm, or quicktime media is supported")
}

// videoISOBrand reports whether an ISO base-media brand identifies a video
// container rather than a still image codec family (heic/heif/avif/mif1...).
func videoISOBrand(brand string) bool {
	switch brand {
	case "heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs",
		"avif", "avis", "mif1", "msf1", "qt  ", "crx ", "jp2 ":
		return false
	}
	switch {
	case strings.HasPrefix(brand, "mp4"), strings.HasPrefix(brand, "isom"),
		strings.HasPrefix(brand, "avc1"), strings.HasPrefix(brand, "dash"),
		strings.HasPrefix(brand, "3gp"), strings.HasPrefix(brand, "mmp4"):
		return true
	}
	return false
}

func validCachedMediaContentPath(path, kind string) bool {
	path = strings.TrimSpace(path)
	kind = strings.ToLower(strings.TrimSpace(kind))
	if path == "" || (kind != "image" && kind != "video") {
		return false
	}
	base, err := filepath.Abs(filepath.Join(cacheBaseDir, kind))
	if err != nil {
		return false
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(base, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

// mediaInputAdminOwner is the namespace the management plane uploads into.
// A management-plane input is a deployment-level asset (the operator uploaded
// it, any client may reference its unguessable id), which is how grok2api's
// admin-plane media inputs behave.
const mediaInputAdminOwner = "admin"

// loadMediaInput resolves an input for the caller: its own namespace first, then
// the management plane's.
func (h *Handler) loadMediaInput(ctx context.Context, id, owner string) (*store.StoredMediaInput, error) {
	input, err := h.lb.Store.GetStoredMediaInput(ctx, id, owner)
	if err == nil {
		return input, nil
	}
	if !errors.Is(err, store.ErrNoRows) || owner == mediaInputAdminOwner {
		return nil, err
	}
	return h.lb.Store.GetStoredMediaInput(ctx, id, mediaInputAdminOwner)
}

func (h *Handler) resolveMediaInputDataURL(ctx context.Context, id, owner, expectedKind string) (string, int64, error) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return "", 0, fmt.Errorf("media input store is not configured")
	}
	input, err := h.loadMediaInput(ctx, id, owner)
	if err != nil {
		if errors.Is(err, store.ErrNoRows) {
			return "", 0, fmt.Errorf("file_id is unavailable or belongs to another API key")
		}
		return "", 0, fmt.Errorf("failed to load file_id")
	}
	if input.Kind != expectedKind {
		return "", 0, fmt.Errorf("file_id must reference %s media", expectedKind)
	}
	if input.SizeBytes <= 0 || input.SizeBytes > maxMediaInputBytes || !validCachedMediaContentPath(input.ContentPath, input.Kind) {
		return "", 0, fmt.Errorf("file_id media is invalid")
	}
	file, err := os.Open(input.ContentPath)
	if err != nil {
		return "", 0, fmt.Errorf("file_id media is unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxMediaInputBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxMediaInputBytes || int64(len(data)) != input.SizeBytes {
		return "", 0, fmt.Errorf("file_id media is invalid")
	}
	kind, mimeType, err := detectMediaInput(data, input.MIMEType)
	if err != nil || kind != expectedKind || !strings.EqualFold(mimeType, input.MIMEType) {
		return "", 0, fmt.Errorf("file_id media is invalid")
	}
	return dataURIFromBytes(mimeType, data), int64(len(data)), nil
}

// HandleAdminMediaInputs is the management-plane upload endpoint. It is the
// same storage as the inference-plane endpoint, exposed with the envelope and
// field names grok2api's admin API uses, so an operator tool written against
// that contract works here too:
//
//	POST /api/media/inputs            multipart "file" -> {"data": {fileId, ...}}
//	GET  /api/media/inputs/{id}       metadata
//	DELETE /api/media/inputs/{id}     delete
//
// Uploads land in the shared admin namespace, so a client request may reference
// the returned file_id without knowing who uploaded it.
func (h *Handler) HandleAdminMediaInputs(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, adminMediaError("service_unavailable", "media input store is not configured"))
		return
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(r.Header.Get("Content-Type")))
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") {
		writeJSONStatus(w, http.StatusUnsupportedMediaType, adminMediaError("invalid_request", "media input upload requires multipart/form-data"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMediaInputRequestBytes)
	if err := r.ParseMultipartForm(maxMediaInputRequestBytes); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONStatus(w, http.StatusRequestEntityTooLarge, adminMediaError("media_too_large", "media input exceeds 20 MiB"))
			return
		}
		writeJSONStatus(w, http.StatusBadRequest, adminMediaError("invalid_request", "invalid media input upload"))
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		writeJSONStatus(w, http.StatusBadRequest, adminMediaError("invalid_request", "exactly one file is required"))
		return
	}
	fileHeader := files[0]
	if fileHeader.Size <= 0 {
		writeJSONStatus(w, http.StatusBadRequest, adminMediaError("invalidMedia", "media input cannot be empty"))
		return
	}
	if fileHeader.Size > maxMediaInputBytes {
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, adminMediaError("mediaTooLarge", "media input exceeds 20 MiB"))
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, adminMediaError("invalid_media", "failed to read media input"))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxMediaInputBytes+1))
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, adminMediaError("invalidMedia", "failed to read media input"))
		return
	}
	if len(data) == 0 {
		writeJSONStatus(w, http.StatusBadRequest, adminMediaError("invalidMedia", "media input cannot be empty"))
		return
	}
	if len(data) > maxMediaInputBytes {
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, adminMediaError("mediaTooLarge", "media input exceeds 20 MiB"))
		return
	}
	input, err := h.saveAdminMediaInput(r.Context(), data, fileHeader.Header.Get("Content-Type"))
	if err != nil {
		status, code, message := adminMediaSaveError(err)
		writeJSONStatus(w, status, adminMediaError(code, message))
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]interface{}{"data": adminMediaInputJSON(input)})
}

// HandleMediaInputImport downloads a remote input into the authenticated API
// key's namespace. It deliberately uses the same SSRF-safe fetcher as the admin
// endpoint, but returns the inference-plane file object rather than an admin
// envelope.
func (h *Handler) HandleMediaInputImport(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		writeResponsesAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "media input store is not configured")
		return
	}
	rawURL, ok := decodeMediaInputImportURL(w, r, false)
	if !ok {
		return
	}
	data, declared, err := adminMediaInputFetcher(r.Context(), rawURL)
	if err != nil {
		switch {
		case errors.Is(err, errAdminMediaTooLarge):
			writeResponsesAPIError(w, http.StatusRequestEntityTooLarge, "media_too_large", errAdminMediaTooLarge.Error())
		case errors.Is(err, errAdminMediaBlocked), errors.Is(err, errRemoteFetchBlocked):
			writeResponsesAPIError(w, http.StatusBadRequest, "media_url_blocked", "media URL is not allowed")
		default:
			writeResponsesAPIError(w, http.StatusBadGateway, "media_fetch_failed", "failed to download media input")
		}
		return
	}
	input, err := h.saveMediaInput(r.Context(), data, declared, videoRequestOwner(r))
	if err != nil {
		status, code, message := adminMediaSaveError(err)
		writeResponsesAPIError(w, status, strings.ToLower(code), message)
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]interface{}{
		"file_id": input.ID, "object": "file", "kind": input.Kind, "mime_type": input.MIMEType,
		"bytes": input.SizeBytes, "created_at": input.CreatedAt.Unix(), "expires_at": input.ExpiresAt.Format(time.RFC3339),
	})
}

// HandleAdminMediaInputImport downloads a remote input without using environment
// proxies. DNS is resolved by the public-only dialer and each redirect is checked
// again, preventing proxy bypass and DNS-rebinding access to internal services.
func (h *Handler) HandleAdminMediaInputImport(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, adminMediaError("serviceUnavailable", "media input store is not configured"))
		return
	}
	rawURL, ok := decodeMediaInputImportURL(w, r, true)
	if !ok {
		return
	}
	data, declared, err := adminMediaInputFetcher(r.Context(), rawURL)
	if err != nil {
		switch {
		case errors.Is(err, errAdminMediaTooLarge):
			writeJSONStatus(w, http.StatusRequestEntityTooLarge, adminMediaError("mediaTooLarge", errAdminMediaTooLarge.Error()))
		case errors.Is(err, errAdminMediaBlocked), errors.Is(err, errRemoteFetchBlocked):
			writeJSONStatus(w, http.StatusBadRequest, adminMediaError("mediaURLBlocked", "media URL is not allowed"))
		default:
			writeJSONStatus(w, http.StatusBadGateway, adminMediaError("mediaFetchFailed", "failed to download media input"))
		}
		return
	}
	input, err := h.saveAdminMediaInput(r.Context(), data, declared)
	if err != nil {
		status, code, message := adminMediaSaveError(err)
		writeJSONStatus(w, status, adminMediaError(code, message))
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]interface{}{"data": adminMediaInputJSON(input)})
}

func decodeMediaInputImportURL(w http.ResponseWriter, r *http.Request, admin bool) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminMediaImportURLBytes+1024)
	var request struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		if admin {
			writeJSONStatus(w, http.StatusBadRequest, adminMediaError("invalidRequest", "invalid JSON request"))
		} else {
			writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request")
		}
		return "", false
	}
	rawURL := strings.TrimSpace(request.URL)
	if rawURL == "" || len(rawURL) > maxAdminMediaImportURLBytes {
		if admin {
			writeJSONStatus(w, http.StatusBadRequest, adminMediaError("invalidMediaURL", "media URL is required and must not exceed 8192 bytes"))
		} else {
			writeResponsesAPIError(w, http.StatusBadRequest, "invalid_media_url", "media URL is required and must not exceed 8192 bytes")
		}
		return "", false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User != nil || parsed.Hostname() == "" ||
		(!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) ||
		(parsed.Port() != "" && parsed.Port() != "80" && parsed.Port() != "443") {
		if admin {
			writeJSONStatus(w, http.StatusBadRequest, adminMediaError("invalidMediaURL", "only credential-free HTTP/HTTPS URLs on ports 80 or 443 are supported"))
		} else {
			writeResponsesAPIError(w, http.StatusBadRequest, "invalid_media_url", "only credential-free HTTP/HTTPS URLs on ports 80 or 443 are supported")
		}
		return "", false
	}
	return rawURL, true
}

func fetchAdminMediaInput(ctx context.Context, rawURL string) ([]byte, string, error) {
	if _, err := checkRemoteFetchTarget(ctx, rawURL, false); err != nil {
		return nil, "", fmt.Errorf("%w: %v", errAdminMediaBlocked, err)
	}
	client := newRemoteFetchClient(20*time.Second, nil)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", "image/*, video/*")
	request.Header.Set("User-Agent", "orchids-media-importer/1.0")
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, errRemoteFetchBlocked) {
			return nil, "", fmt.Errorf("%w: %v", errAdminMediaBlocked, err)
		}
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("remote server returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxMediaInputBytes {
		return nil, "", errAdminMediaTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMediaInputBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxMediaInputBytes {
		return nil, "", errAdminMediaTooLarge
	}
	return data, response.Header.Get("Content-Type"), nil
}

func (h *Handler) saveAdminMediaInput(ctx context.Context, data []byte, declaredMIME string) (*store.StoredMediaInput, error) {
	return h.saveMediaInput(ctx, data, declaredMIME, mediaInputAdminOwner)
}

func (h *Handler) saveMediaInput(ctx context.Context, data []byte, declaredMIME, owner string) (*store.StoredMediaInput, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("invalid media: media input cannot be empty")
	}
	if len(data) > maxMediaInputBytes {
		return nil, errAdminMediaTooLarge
	}
	kind, mimeType, err := detectMediaInput(data, declaredMIME)
	if err != nil {
		return nil, fmt.Errorf("invalid media: %w", err)
	}
	if !reserveMediaInputBytes(int64(len(data))) {
		return nil, fmt.Errorf("media storage full")
	}
	reserved := true
	defer func() {
		if reserved {
			releaseMediaInputBytes(int64(len(data)))
		}
	}()
	id, err := newMediaInputID()
	if err != nil {
		return nil, fmt.Errorf("allocate media input: %w", err)
	}
	name, err := h.cacheMediaInputBytes(id, kind, data, mimeType)
	if err != nil {
		return nil, fmt.Errorf("cache media input: %w", err)
	}
	contentPath := filepath.Join(cacheBaseDir, kind, name)
	now := time.Now().UTC()
	input := &store.StoredMediaInput{
		ID: id, OwnerHash: strings.TrimSpace(owner), Kind: kind, MIMEType: mimeType,
		ContentPath: contentPath, SizeBytes: int64(len(data)), CreatedAt: now,
	}
	if err := h.lb.Store.SaveStoredMediaInput(ctx, input, mediaInputTTL); err != nil {
		_ = os.Remove(contentPath)
		return nil, fmt.Errorf("persist media input: %w", err)
	}
	reserved = false
	return input, nil
}

func adminMediaSaveError(err error) (int, string, string) {
	switch {
	case errors.Is(err, errAdminMediaTooLarge):
		return http.StatusRequestEntityTooLarge, "mediaTooLarge", errAdminMediaTooLarge.Error()
	case strings.HasPrefix(err.Error(), "invalid media:"):
		return http.StatusBadRequest, "invalidMedia", strings.TrimSpace(strings.TrimPrefix(err.Error(), "invalid media:"))
	case strings.Contains(err.Error(), "storage full"):
		return http.StatusInsufficientStorage, "mediaStorageFull", "media input storage is full"
	case strings.HasPrefix(err.Error(), "persist media input:"):
		return http.StatusServiceUnavailable, "serviceUnavailable", "failed to persist media input"
	default:
		return http.StatusInternalServerError, "internalError", "failed to save media input"
	}
}

// HandleAdminMediaInputResource serves GET/DELETE for a management-plane input.
func (h *Handler) HandleAdminMediaInputResource(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, adminMediaError("service_unavailable", "media input store is not configured"))
		return
	}
	id := strings.Trim(strings.TrimPrefix(strings.TrimSpace(r.URL.Path), "/api/media/inputs/"), "/")
	if !validMediaInputID(id) {
		writeJSONStatus(w, http.StatusNotFound, adminMediaError("not_found", "media input not found"))
		return
	}
	input, err := h.lb.Store.GetStoredMediaInput(r.Context(), id, mediaInputAdminOwner)
	if err != nil {
		writeJSONStatus(w, http.StatusNotFound, adminMediaError("not_found", "media input not found"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]interface{}{"data": adminMediaInputJSON(input)})
	case http.MethodDelete:
		if err := h.lb.Store.DeleteStoredMediaInput(r.Context(), id, mediaInputAdminOwner); err != nil {
			writeJSONStatus(w, http.StatusNotFound, adminMediaError("not_found", "media input not found"))
			return
		}
		if validCachedMediaContentPath(input.ContentPath, input.Kind) {
			_ = os.Remove(input.ContentPath)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSONStatus(w, http.StatusMethodNotAllowed, adminMediaError("method_not_allowed", "method not allowed"))
	}
}

// adminMediaInputJSON renders an input with the management contract's names.
func adminMediaInputJSON(input *store.StoredMediaInput) map[string]interface{} {
	if input == nil {
		return nil
	}
	return map[string]interface{}{
		"id":        input.ID,
		"fileId":    input.ID,
		"kind":      input.Kind,
		"mimeType":  input.MIMEType,
		"sizeBytes": input.SizeBytes,
		"createdAt": input.CreatedAt.UTC().Format(time.RFC3339),
		"expiresAt": input.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

// adminMediaError renders the management contract's error object.
func adminMediaError(code, message string) map[string]interface{} {
	return map[string]interface{}{"error": map[string]interface{}{"code": code, "message": message}}
}
