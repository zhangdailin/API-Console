package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"orchids-api/internal/config"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/responses"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
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
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		util.WriteJSONStatus(w, http.StatusServiceUnavailable, adminMediaError("service_unavailable", "media input store is not configured"))
		return
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(r.Header.Get("Content-Type")))
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") {
		util.WriteJSONStatus(w, http.StatusUnsupportedMediaType, adminMediaError("invalid_request", "media input upload requires multipart/form-data"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMediaInputRequestBytes)
	if err := r.ParseMultipartForm(maxMediaInputRequestBytes); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			util.WriteJSONStatus(w, http.StatusRequestEntityTooLarge, adminMediaError("media_too_large", "media input exceeds 20 MiB"))
			return
		}
		util.WriteJSONStatus(w, http.StatusBadRequest, adminMediaError("invalid_request", "invalid media input upload"))
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		util.WriteJSONStatus(w, http.StatusBadRequest, adminMediaError("invalid_request", "exactly one file is required"))
		return
	}
	fileHeader := files[0]
	if fileHeader.Size <= 0 {
		util.WriteJSONStatus(w, http.StatusBadRequest, adminMediaError("invalidMedia", "media input cannot be empty"))
		return
	}
	if fileHeader.Size > maxMediaInputBytes {
		util.WriteJSONStatus(w, http.StatusRequestEntityTooLarge, adminMediaError("mediaTooLarge", "media input exceeds 20 MiB"))
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		util.WriteJSONStatus(w, http.StatusBadRequest, adminMediaError("invalid_media", "failed to read media input"))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxMediaInputBytes+1))
	if err != nil {
		util.WriteJSONStatus(w, http.StatusBadRequest, adminMediaError("invalidMedia", "failed to read media input"))
		return
	}
	if len(data) == 0 {
		util.WriteJSONStatus(w, http.StatusBadRequest, adminMediaError("invalidMedia", "media input cannot be empty"))
		return
	}
	if len(data) > maxMediaInputBytes {
		util.WriteJSONStatus(w, http.StatusRequestEntityTooLarge, adminMediaError("mediaTooLarge", "media input exceeds 20 MiB"))
		return
	}
	input, err := h.saveAdminMediaInput(r.Context(), data, fileHeader.Header.Get("Content-Type"))
	if err != nil {
		status, code, message := adminMediaSaveError(err)
		util.WriteJSONStatus(w, status, adminMediaError(code, message))
		return
	}
	util.WriteJSONStatus(w, http.StatusCreated, map[string]any{"data": adminMediaInputJSON(input)})
}

// HandleMediaInputImport downloads a remote input into the authenticated API
// key's namespace. It deliberately uses the same SSRF-safe fetcher as the admin
// endpoint, but returns the inference-plane file object rather than an admin
// envelope.
func (h *Handler) HandleMediaInputImport(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		responses.WriteAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "media input store is not configured")
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
			responses.WriteAPIError(w, http.StatusRequestEntityTooLarge, "media_too_large", errAdminMediaTooLarge.Error())
		case errors.Is(err, errAdminMediaBlocked), errors.Is(err, errRemoteFetchBlocked):
			responses.WriteAPIError(w, http.StatusBadRequest, "media_url_blocked", "media URL is not allowed")
		default:
			responses.WriteAPIError(w, http.StatusBadGateway, "media_fetch_failed", "failed to download media input")
		}
		return
	}
	input, err := h.saveMediaInput(r.Context(), data, declared, videoRequestOwner(r))
	if err != nil {
		status, code, message := adminMediaSaveError(err)
		responses.WriteAPIError(w, status, strings.ToLower(code), message)
		return
	}
	util.WriteJSONStatus(w, http.StatusCreated, map[string]any{
		"file_id": input.ID, "object": "file", "kind": input.Kind, "mime_type": input.MIMEType,
		"bytes": input.SizeBytes, "created_at": input.CreatedAt.Unix(), "expires_at": input.ExpiresAt.Format(time.RFC3339),
	})
}

// HandleAdminMediaInputImport downloads a remote input without using environment
// proxies. DNS is resolved by the public-only dialer and each redirect is checked
// again, preventing proxy bypass and DNS-rebinding access to internal services.
func (h *Handler) HandleAdminMediaInputImport(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		util.WriteJSONStatus(w, http.StatusServiceUnavailable, adminMediaError("serviceUnavailable", "media input store is not configured"))
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
			util.WriteJSONStatus(w, http.StatusRequestEntityTooLarge, adminMediaError("mediaTooLarge", errAdminMediaTooLarge.Error()))
		case errors.Is(err, errAdminMediaBlocked), errors.Is(err, errRemoteFetchBlocked):
			util.WriteJSONStatus(w, http.StatusBadRequest, adminMediaError("mediaURLBlocked", "media URL is not allowed"))
		default:
			util.WriteJSONStatus(w, http.StatusBadGateway, adminMediaError("mediaFetchFailed", "failed to download media input"))
		}
		return
	}
	input, err := h.saveAdminMediaInput(r.Context(), data, declared)
	if err != nil {
		status, code, message := adminMediaSaveError(err)
		util.WriteJSONStatus(w, status, adminMediaError(code, message))
		return
	}
	util.WriteJSONStatus(w, http.StatusCreated, map[string]any{"data": adminMediaInputJSON(input)})
}

func decodeMediaInputImportURL(w http.ResponseWriter, r *http.Request, admin bool) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminMediaImportURLBytes+1024)
	var request struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		if admin {
			util.WriteJSONStatus(w, http.StatusBadRequest, adminMediaError("invalidRequest", "invalid JSON request"))
		} else {
			responses.WriteAPIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request")
		}
		return "", false
	}
	rawURL := strings.TrimSpace(request.URL)
	if rawURL == "" || len(rawURL) > maxAdminMediaImportURLBytes {
		if admin {
			util.WriteJSONStatus(w, http.StatusBadRequest, adminMediaError("invalidMediaURL", "media URL is required and must not exceed 8192 bytes"))
		} else {
			responses.WriteAPIError(w, http.StatusBadRequest, "invalid_media_url", "media URL is required and must not exceed 8192 bytes")
		}
		return "", false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User != nil || parsed.Hostname() == "" ||
		(!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) ||
		(parsed.Port() != "" && parsed.Port() != "80" && parsed.Port() != "443") {
		if admin {
			util.WriteJSONStatus(w, http.StatusBadRequest, adminMediaError("invalidMediaURL", "only credential-free HTTP/HTTPS URLs on ports 80 or 443 are supported"))
		} else {
			responses.WriteAPIError(w, http.StatusBadRequest, "invalid_media_url", "only credential-free HTTP/HTTPS URLs on ports 80 or 443 are supported")
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
	// Many image hosts and CDNs refuse non-browser client identities.
	request.Header.Set("User-Agent", config.DefaultGrokWebUserAgent)
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
		util.WriteJSONStatus(w, http.StatusServiceUnavailable, adminMediaError("service_unavailable", "media input store is not configured"))
		return
	}
	id := strings.Trim(strings.TrimPrefix(strings.TrimSpace(r.URL.Path), "/api/media/inputs/"), "/")
	if !validMediaInputID(id) {
		util.WriteJSONStatus(w, http.StatusNotFound, adminMediaError("not_found", "media input not found"))
		return
	}
	input, err := h.lb.Store.GetStoredMediaInput(r.Context(), id, mediaInputAdminOwner)
	if err != nil {
		util.WriteJSONStatus(w, http.StatusNotFound, adminMediaError("not_found", "media input not found"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		util.WriteJSON(w, map[string]any{"data": adminMediaInputJSON(input)})
	case http.MethodDelete:
		if err := h.lb.Store.DeleteStoredMediaInput(r.Context(), id, mediaInputAdminOwner); err != nil {
			util.WriteJSONStatus(w, http.StatusNotFound, adminMediaError("not_found", "media input not found"))
			return
		}
		if validCachedMediaContentPath(input.ContentPath, input.Kind) {
			_ = os.Remove(input.ContentPath)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		util.WriteJSONStatus(w, http.StatusMethodNotAllowed, adminMediaError("method_not_allowed", "method not allowed"))
	}
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
