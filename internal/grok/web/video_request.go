package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"orchids-api/internal/chatwire"
	"os"
	"sort"
	"strings"
)

func parseVideosRequest(r *http.Request) (VideosRequest, error) {
	var req VideosRequest
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(80 << 20); err != nil {
			return req, err
		}
		refs, err := readVideoInputReferenceFiles(r)
		if err != nil {
			return req, err
		}
		req.InputReferences = refs
		fillVideosRequestFromForm(&req, r.Form)
		return req, nil
	}
	if strings.Contains(contentType, "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return req, err
		}
		fillVideosRequestFromForm(&req, r.Form)
		return req, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxVideoJSONBytes+1))
	if err != nil {
		return req, err
	}
	if len(raw) > maxVideoJSONBytes {
		return req, fmt.Errorf("video request exceeds %d MiB", maxVideoJSONBytes>>20)
	}
	// Unknown keys are rejected so a typo ("duratoin") cannot silently fall back
	// to the defaults. The check is explicit because the JSON implementation in
	// use does not enforce Decoder.DisallowUnknownFields.
	if err := rejectUnknownVideoFields(raw); err != nil {
		return req, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&req); err != nil {
		return req, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return req, fmt.Errorf("unexpected trailing content in video request")
	}
	normalizeVideoRequestAliases(&req)
	return req, nil
}

// maxVideoJSONBytes bounds the JSON video body. Media travels as URLs or file
// ids here, so a huge body is never legitimate.
const maxVideoJSONBytes = 16 << 20

// videoRequestFields is every key this endpoint accepts on the JSON surface.
var videoRequestFields = map[string]bool{
	"model": true, "prompt": true,
	"seconds": true, "duration": true, "video_length": true,
	"size": true, "aspect_ratio": true,
	"resolution_name": true, "resolution": true,
	"preset": true, "user": true,
	"image": true, "video": true,
	"reference_images": true, "reference_audios": true,
	"input_reference": true, "input_references": true,
	"input_reference[]": true,
}

// rejectUnknownVideoFields fails on any key outside the accepted set, naming the
// offending field so a client can fix the typo.
func rejectUnknownVideoFields(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	unknown := make([]string, 0, len(fields))
	for key := range fields {
		if !videoRequestFields[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return &unknownVideoFieldError{field: unknown[0]}
}

// unknownVideoFieldError names a JSON field the video endpoint does not
// accept. The name is the caller's own input, so it is safe to echo.
type unknownVideoFieldError struct{ field string }

func (e *unknownVideoFieldError) Error() string {
	return fmt.Sprintf("unknown video request field %q", e.field)
}

// normalizeVideoRequestAliases folds the grok2api-compatible spellings onto the
// canonical fields the rest of the handler uses.
func normalizeVideoRequestAliases(req *VideosRequest) {
	if req == nil {
		return
	}
	if req.Seconds == 0 && len(req.Duration) > 0 {
		if seconds, err := chatwire.ParseLooseIntAny(req.Duration); err == nil && seconds > 0 {
			req.Seconds = seconds
		}
	}
	if strings.TrimSpace(req.Size) == "" {
		req.Size = strings.TrimSpace(req.AspectRatio)
	}
	if strings.TrimSpace(req.ResolutionName) == "" {
		req.ResolutionName = strings.TrimSpace(req.Resolution)
	}
	appendReference := func(value string) {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			req.InputReferences = append(req.InputReferences, trimmed)
		}
	}
	appendReference(req.Image)
	appendReference(req.Video)
	for _, value := range req.ReferenceImages {
		appendReference(value)
	}
	req.InputReferences = uniqueStrings(req.InputReferences)
}

// fillVideosRequestFromForm copies the video request fields from form values.
// Multipart and urlencoded bodies share the same field layout.
func fillVideosRequestFromForm(req *VideosRequest, form url.Values) {
	req.Model = strings.TrimSpace(form.Get("model"))
	req.Prompt = strings.TrimSpace(form.Get("prompt"))
	req.Seconds = parseIntLoose(firstNonEmpty(form.Get("seconds"), form.Get("duration"), form.Get("video_length")), 8)
	req.Size = strings.TrimSpace(firstNonEmpty(form.Get("size"), form.Get("aspect_ratio")))
	req.ResolutionName = strings.TrimSpace(firstNonEmpty(form.Get("resolution_name"), form.Get("resolution")))
	req.Preset = strings.TrimSpace(form.Get("preset"))
	for _, key := range []string{"input_reference", "input_references", "input_reference[]"} {
		for _, value := range form[key] {
			if s := strings.TrimSpace(value); s != "" {
				req.InputReferences = append(req.InputReferences, s)
			}
		}
	}
	req.InputReferences = uniqueStrings(req.InputReferences)
}

func readVideoInputReferenceFiles(r *http.Request) ([]string, error) {
	if r.MultipartForm == nil {
		return nil, nil
	}
	var out []string
	for _, key := range []string{"input_reference", "input_reference[]"} {
		for _, fh := range r.MultipartForm.File[key] {
			dataURI, err := uploadFileHeaderToDataURI(fh)
			if err != nil {
				return nil, err
			}
			out = append(out, dataURI)
		}
	}
	return out, nil
}

func uploadFileHeaderToDataURI(fh *multipart.FileHeader) (string, error) {
	file, err := fh.Open()
	if err != nil {
		return "", fmt.Errorf("failed to read input_reference")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxEditImageBytes+1))
	if err != nil {
		return "", fmt.Errorf("failed to read input_reference")
	}
	if len(data) == 0 {
		return "", fmt.Errorf("input_reference cannot be empty")
	}
	if len(data) > maxEditImageBytes {
		return "", fmt.Errorf("input_reference too large. maximum is 50MB")
	}
	mime := strings.TrimSpace(fh.Header.Get("Content-Type"))
	if mime == "" || mime == "application/octet-stream" {
		mime = mimeFromFilename(fh.Filename)
	}
	if !strings.HasPrefix(strings.ToLower(mime), "image/") {
		return "", fmt.Errorf("input_reference must be an image")
	}
	return dataURIFromBytes(mime, data), nil
}

func (h *Handler) HandleVideosRetrieve(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	videoID := videoIDFromPath(r.URL.Path)
	if idx := strings.Index(videoID, "/"); idx >= 0 {
		videoID = videoID[:idx]
	}
	job, ok := h.lookupVideoJob(r.Context(), videoID, videoRequestOwner(r))
	if !ok {
		writeGrokError(w, http.StatusNotFound, "video not found")
		return
	}
	writeJSON(w, job.toMap())
}

func (h *Handler) HandleVideosContent(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	videoID := videoIDFromPath(r.URL.Path)
	job, ok := h.lookupVideoJob(r.Context(), videoID, videoRequestOwner(r))
	if !ok {
		writeGrokError(w, http.StatusNotFound, "video not found")
		return
	}
	if job.Status != "completed" || strings.TrimSpace(job.ContentPath) == "" {
		writeGrokError(w, http.StatusConflict, "video content is not ready yet")
		return
	}
	file, err := os.Open(job.ContentPath)
	if err != nil {
		writeGrokError(w, http.StatusNotFound, "video content not found")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeGrokError(w, http.StatusNotFound, "video content not found")
		return
	}
	// Stream the file instead of reading it whole: a finished video can be
	// hundreds of megabytes, and several concurrent downloads used to multiply
	// that in memory. ServeContent also gives Range requests and conditional
	// GETs for free.
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", `inline; filename="`+sanitizeCachedFilename(videoID)+`.mp4"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, videoID+".mp4", info.ModTime(), file)
}

func videoIDFromPath(path string) string {
	path = strings.TrimSpace(path)
	for _, prefix := range []string{"/api/grok/tools/v1/videos/", "/grok/v1/videos/", "/v1/videos/"} {
		if after, ok := strings.CutPrefix(path, prefix); ok {
			rest := strings.Trim(after, "/")
			rest = strings.TrimPrefix(rest, "generations/")
			rest = strings.TrimSuffix(rest, "/content")
			rest = strings.Trim(rest, "/")
			if idx := strings.Index(rest, "/"); idx >= 0 {
				rest = rest[:idx]
			}
			return rest
		}
	}
	return ""
}
