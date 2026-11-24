package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/store"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
	h.handleMediaUpload(w, r, true)
}

func (h *Handler) HandleMediaInputImport(w http.ResponseWriter, r *http.Request) {
	h.handleMediaInputImport(w, r, false)
}

func (h *Handler) HandleAdminMediaInputImport(w http.ResponseWriter, r *http.Request) {
	h.handleMediaInputImport(w, r, true)
}

// Both routes use the guarded fetcher. The owner and response format come from
// the route, never from any field supplied with the URL.
func (h *Handler) handleMediaInputImport(w http.ResponseWriter, r *http.Request, admin bool) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		writeMediaInputError(w, admin, http.StatusServiceUnavailable, "service_unavailable", "serviceUnavailable", "media input store is not configured")
		return
	}
	rawURL, ok := decodeMediaInputImportURL(w, r, admin)
	if !ok {
		return
	}
	data, declared, err := adminMediaInputFetcher(r.Context(), rawURL)
	if err != nil {
		switch {
		case errors.Is(err, errAdminMediaTooLarge):
			writeMediaInputError(w, admin, http.StatusRequestEntityTooLarge, "media_too_large", "mediaTooLarge", errAdminMediaTooLarge.Error())
		case errors.Is(err, errAdminMediaBlocked), errors.Is(err, errRemoteFetchBlocked):
			writeMediaInputError(w, admin, http.StatusBadRequest, "media_url_blocked", "mediaURLBlocked", "media URL is not allowed")
		default:
			writeMediaInputError(w, admin, http.StatusBadGateway, "media_fetch_failed", "mediaFetchFailed", "failed to download media input")
		}
		return
	}
	h.saveRequestMediaInput(w, r, data, declared, admin, false)
}

func decodeMediaInputImportURL(w http.ResponseWriter, r *http.Request, admin bool) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminMediaImportURLBytes+1024)
	var request struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeMediaInputError(w, admin, http.StatusBadRequest, "invalid_request", "invalidRequest", "invalid JSON request")
		return "", false
	}
	rawURL := strings.TrimSpace(request.URL)
	if rawURL == "" || len(rawURL) > maxAdminMediaImportURLBytes {
		writeMediaInputError(w, admin, http.StatusBadRequest, "invalid_media_url", "invalidMediaURL", "media URL is required and must not exceed 8192 bytes")
		return "", false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User != nil || parsed.Hostname() == "" ||
		(!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) ||
		(parsed.Port() != "" && parsed.Port() != "80" && parsed.Port() != "443") {
		writeMediaInputError(w, admin, http.StatusBadRequest, "invalid_media_url", "invalidMediaURL", "only credential-free HTTP/HTTPS URLs on ports 80 or 443 are supported")
		return "", false
	}
	return rawURL, true
}

func fetchAdminMediaInput(ctx context.Context, rawURL string) ([]byte, string, error) {
	raw, contentType, err := fetchRemoteMedia(ctx, rawURL, 20*time.Second, nil, maxMediaInputBytes, "image/*, video/*", true)
	if errors.Is(err, errRemoteMediaTooLarge) {
		return nil, "", errAdminMediaTooLarge
	}
	if errors.Is(err, errRemoteFetchBlocked) {
		return nil, "", fmt.Errorf("%w: %v", errAdminMediaBlocked, err)
	}
	return raw, contentType, err
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
	// Once this request returns, the directory scan counts the cached file.
	defer releaseMediaInputBytes(int64(len(data)))
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
		ContentPath: contentPath, SizeBytes: int64(len(data)), CreatedAt: now, ExpiresAt: now.Add(mediaInputTTL),
	}
	if err := h.lb.Store.SaveStoredMediaInput(ctx, input, mediaInputTTL); err != nil {
		_ = os.Remove(contentPath)
		return nil, fmt.Errorf("persist media input: %w", err)
	}
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
	h.handleMediaInputResource(w, r, true)
}

// adminMediaInputJSON renders an input with the management contract's names.
func adminMediaInputJSON(input *store.StoredMediaInput) map[string]any {
	if input == nil {
		return nil
	}
	return map[string]any{
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
func adminMediaError(code, message string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": message}}
}
