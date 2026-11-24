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
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/httpserver"
	"orchids-api/internal/responses"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

const (
	mediaInputTTL             = 24 * time.Hour
	maxMediaInputBytes        = 20 << 20
	maxMediaInputRequestBytes = maxMediaInputBytes + (1 << 20)
	maxResolvedMediaBytes     = 32 << 20
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
	h.handleMediaUpload(w, r, false)
}

func (h *Handler) handleMediaUpload(w http.ResponseWriter, r *http.Request, admin bool) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		writeMediaInputError(w, admin, http.StatusServiceUnavailable, "service_unavailable", "service_unavailable", "media input store is not configured")
		return
	}
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	multipartForm := strings.HasPrefix(strings.ToLower(contentType), "multipart/form-data")
	if admin {
		mediaType, _, err := mime.ParseMediaType(contentType)
		multipartForm = err == nil && strings.EqualFold(mediaType, "multipart/form-data")
	}
	if !multipartForm {
		writeMediaInputError(w, admin, http.StatusUnsupportedMediaType, "invalid_request", "invalid_request", "media input upload requires multipart/form-data")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMediaInputRequestBytes)
	if err := r.ParseMultipartForm(maxMediaInputRequestBytes); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeMediaInputError(w, admin, http.StatusRequestEntityTooLarge, "media_too_large", "media_too_large", errAdminMediaTooLarge.Error())
			return
		}
		writeMediaInputError(w, admin, http.StatusBadRequest, "invalid_request", "invalid_request", "invalid media input upload")
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		writeMediaInputError(w, admin, http.StatusBadRequest, "invalid_request", "invalid_request", "exactly one file is required")
		return
	}
	part := files[0]
	if part.Size <= 0 {
		writeMediaInputError(w, admin, http.StatusBadRequest, "invalid_media", "invalidMedia", "media input cannot be empty")
		return
	}
	if part.Size > maxMediaInputBytes {
		writeMediaInputError(w, admin, http.StatusRequestEntityTooLarge, "media_too_large", "mediaTooLarge", errAdminMediaTooLarge.Error())
		return
	}
	file, err := part.Open()
	if err != nil {
		writeMediaInputError(w, admin, http.StatusBadRequest, "invalid_media", "invalid_media", "failed to read media input")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxMediaInputBytes+1))
	if err != nil {
		writeMediaInputError(w, admin, http.StatusBadRequest, "invalid_media", "invalidMedia", "failed to read media input")
		return
	}
	h.saveRequestMediaInput(w, r, data, part.Header.Get("Content-Type"), admin, true)
}

func (h *Handler) saveRequestMediaInput(w http.ResponseWriter, r *http.Request, data []byte, declared string, admin, upload bool) {
	owner := mediaInputAdminOwner
	if !admin {
		owner = videoRequestOwner(r)
	}
	input, err := h.saveMediaInput(r.Context(), data, declared, owner)
	if err != nil {
		status, adminCode, message := adminMediaSaveError(err)
		code := strings.ToLower(adminCode)
		if !admin && upload {
			switch status {
			case http.StatusRequestEntityTooLarge:
				code = "media_too_large"
			case http.StatusBadRequest:
				code = "invalid_media"
			case http.StatusServiceUnavailable:
				code = "service_unavailable"
			case http.StatusInsufficientStorage:
				code, message = "media_storage_full", "media input storage is full; retry after the expired inputs are reclaimed"
			default:
				code, message = "internal_error", "failed to cache media input"
				if strings.HasPrefix(err.Error(), "allocate media input:") {
					message = "failed to allocate media input"
				}
			}
		}
		writeMediaInputError(w, admin, status, code, adminCode, message)
		return
	}
	if admin {
		util.WriteJSONStatus(w, http.StatusCreated, map[string]any{"data": adminMediaInputJSON(input)})
		return
	}
	util.WriteJSONStatus(w, http.StatusCreated, mediaInputJSON(input))
}

func writeMediaInputError(w http.ResponseWriter, admin bool, status int, code, adminCode, message string) {
	if admin {
		util.WriteJSONStatus(w, status, adminMediaError(adminCode, message))
	} else {
		responses.WriteAPIError(w, status, code, message)
	}
}

func mediaInputJSON(input *store.StoredMediaInput) map[string]any {
	return map[string]any{
		"file_id": input.ID, "object": "file", "kind": input.Kind, "mime_type": input.MIMEType,
		"bytes": input.SizeBytes, "created_at": input.CreatedAt.Unix(), "expires_at": input.ExpiresAt.Format(time.RFC3339),
	}
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
	return writeMediaCacheFile(kind, data, func(mediaType string) string {
		sum := sha1.Sum([]byte("input:" + strings.TrimSpace(id)))
		return mediaInputFilePrefix + hex.EncodeToString(sum[:]) + mediaExtFromMime(mediaType, mimeType, "")
	})
}

func (h *Handler) HandleMediaInputResource(w http.ResponseWriter, r *http.Request) {
	h.handleMediaInputResource(w, r, false)
}

func (h *Handler) handleMediaInputResource(w http.ResponseWriter, r *http.Request, admin bool) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		writeMediaInputError(w, admin, http.StatusServiceUnavailable, "service_unavailable", "service_unavailable", "media input store is not configured")
		return
	}
	fail := func(status int, code, message string) {
		if admin {
			util.WriteJSONStatus(w, status, adminMediaError(code, message))
		} else {
			httpserver.WriteError(w, status, message)
		}
	}
	id, owner := mediaInputIDFromPath(r.URL.Path), videoRequestOwner(r)
	if admin {
		id = strings.Trim(strings.TrimPrefix(strings.TrimSpace(r.URL.Path), "/api/media/inputs/"), "/")
		owner = mediaInputAdminOwner
	}
	var input *store.StoredMediaInput
	var err error
	if validMediaInputID(id) {
		input, err = h.loadMediaInput(r.Context(), id, owner)
	}
	if err != nil || input == nil {
		fail(http.StatusNotFound, "not_found", "media input not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if admin {
			util.WriteJSON(w, map[string]any{"data": adminMediaInputJSON(input)})
		} else {
			util.WriteJSON(w, mediaInputJSON(input))
		}
	case http.MethodDelete:
		// A public request may read an admin-owned input, but deletes only
		// from the requesting owner's namespace.
		if err := h.lb.Store.DeleteStoredMediaInput(r.Context(), id, owner); err != nil {
			fail(http.StatusNotFound, "not_found", "media input not found")
			return
		}
		if validCachedMediaContentPath(input.ContentPath, input.Kind) {
			_ = os.Remove(input.ContentPath)
		}
		if admin {
			w.WriteHeader(http.StatusNoContent)
		} else {
			util.WriteJSON(w, map[string]any{"id": id, "object": "file", "deleted": true})
		}
	default:
		fail(http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
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
		if after, ok := strings.CutPrefix(path, prefix); ok {
			return strings.Trim(after, "/")
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
