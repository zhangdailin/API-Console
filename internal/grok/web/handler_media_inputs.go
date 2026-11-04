package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
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
