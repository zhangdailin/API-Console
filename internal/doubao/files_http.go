package doubao

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

func (h *MediaHandler) Files(w http.ResponseWriter, r *http.Request)       { h.uploadFile(w, r, false) }
func (h *MediaHandler) ImageUpload(w http.ResponseWriter, r *http.Request) { h.uploadFile(w, r, true) }

func (h *MediaHandler) uploadFile(w http.ResponseWriter, r *http.Request, imageOnly bool) {
	if !requirePost(w, r) {
		return
	}
	owner := middleware.APIKeyFingerprint(r.Context())
	if owner == "" {
		writeDoubaoError(w, http.StatusUnauthorized, "invalid_api_key", "A managed API key is required for Doubao uploads")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, store.DoubaoFileLimit+(64<<10))
	reader, err := r.MultipartReader()
	if err != nil {
		writeInputError(w, err, "Expected a multipart upload")
		return
	}
	file := store.DoubaoFile{ID: "file_db_" + randomHex(16), CreatedAt: time.Now().Unix()}
	for parts := 0; ; parts++ {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || parts >= 6 {
			writeInputError(w, err, "Invalid multipart upload")
			return
		}
		switch part.FormName() {
		case "file", "image":
			if file.Data != nil || part.FileName() == "" {
				writeInputError(w, nil, "Upload exactly one file")
				return
			}
			file.Name = safeAttachmentName(part.FileName())
			file.Data, err = readAttachment(part, store.DoubaoFileLimit)
			if err != nil {
				writeInputError(w, err, "The Doubao upload is empty or invalid")
				return
			}
		case "purpose":
			if _, err := io.Copy(io.Discard, io.LimitReader(part, 257)); err != nil {
				writeInputError(w, err, "Invalid upload purpose")
				return
			}
		default:
			writeInputError(w, nil, "Unexpected multipart field")
			return
		}
		part.Close()
	}
	if len(file.Data) == 0 {
		writeInputError(w, nil, "A multipart file is required")
		return
	}
	_, file.Image = imageNameFromBytes(file.Data)
	if imageOnly && !file.Image {
		writeInputError(w, nil, "Expected a PNG, JPEG, GIF, or WebP image")
		return
	}
	if err := h.store.SaveDoubaoFile(r.Context(), owner, file); err != nil {
		if errors.Is(err, store.ErrDoubaoFileLimit) {
			writeDoubaoError(w, http.StatusTooManyRequests, "rate_limit_error", "Too many active Doubao uploads; delete a file or wait for expiry")
			return
		}
		writeDoubaoError(w, http.StatusServiceUnavailable, "service_unavailable", "Could not store the Doubao upload")
		return
	}
	writeFileObject(w, file.Metadata())
}

func (h *MediaHandler) FileResource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("file_id")
	owner := middleware.APIKeyFingerprint(r.Context())
	if owner == "" || !strings.HasPrefix(id, "file_db_") || len(id) != len("file_db_")+32 {
		writeDoubaoError(w, http.StatusNotFound, "not_found", "Doubao file not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		file, err := h.store.GetDoubaoFileMetadata(r.Context(), owner, id)
		if fileResourceError(w, err) {
			return
		}
		writeFileObject(w, file)
	case http.MethodDelete:
		if fileResourceError(w, h.store.DeleteDoubaoFile(r.Context(), owner, id)) {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		util.WriteJSONStatus(w, http.StatusOK, map[string]any{"id": id, "object": "file", "deleted": true})
	default:
		w.Header().Set("Allow", "GET, DELETE")
		writeDoubaoError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
	}
}

func fileResourceError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrNoRows) {
		writeDoubaoError(w, http.StatusNotFound, "not_found", "Doubao file not found")
	} else {
		writeDoubaoError(w, http.StatusServiceUnavailable, "service_unavailable", "Doubao file storage is unavailable")
	}
	return true
}

func writeFileObject(w http.ResponseWriter, file store.DoubaoFileMetadata) {
	kind := "file"
	if file.Image {
		kind = "image"
	}
	w.Header().Set("Cache-Control", "no-store")
	util.WriteJSONStatus(w, http.StatusOK, map[string]any{
		"id": file.ID, "object": "file", "filename": file.Name, "bytes": file.Size, "type": kind,
		"purpose": "user_data", "created_at": file.CreatedAt, "expires_at": file.CreatedAt + int64(store.DoubaoFileTTL.Seconds()),
	})
}
