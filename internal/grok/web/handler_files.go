package web

import (
	"fmt"
	"mime"
	"net/http"
	"orchids-api/internal/httpserver"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func sanitizeCachedFilename(raw string) string {
	name := strings.TrimSpace(raw)
	name = strings.ReplaceAll(name, "\\", "-")
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "..") {
		return ""
	}
	return name
}

func parseFilesPath(rawPath string) (mediaType string, fileName string, ok bool) {
	prefix := ""
	switch {
	case strings.HasPrefix(rawPath, "/api/grok/tools/v1/files/"):
		prefix = "/api/grok/tools/v1/files/"
	case strings.HasPrefix(rawPath, "/grok/v1/files/"):
		prefix = "/grok/v1/files/"
	case strings.HasPrefix(rawPath, "/v1/files/"):
		prefix = "/v1/files/"
	default:
		return "", "", false
	}
	path := strings.TrimPrefix(rawPath, prefix)
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", false
	}
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	mediaType = strings.ToLower(strings.TrimSpace(parts[0]))
	if mediaType != "image" && mediaType != "video" {
		return "", "", false
	}
	fileName = sanitizeCachedFilename(parts[1])
	if fileName == "" {
		return "", "", false
	}
	return mediaType, fileName, true
}

func (h *Handler) HandleFiles(w http.ResponseWriter, r *http.Request) {
	// HEAD is the probe a client (and grok2api's /v1/media endpoint) uses to
	// check a cached asset exists before downloading it; refusing it forces a
	// full transfer for every existence check.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		httpserver.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	mediaType, fileName, ok := parseFilesPath(r.URL.Path)
	if !ok {
		httpserver.WriteError(w, http.StatusNotFound, "file not found")
		return
	}

	fullPath := filepath.Join(cacheBaseDir, mediaType, fileName)
	info, err := os.Stat(fullPath)
	if err != nil || !info.Mode().IsRegular() {
		httpserver.WriteError(w, http.StatusNotFound, "file not found")
		return
	}

	ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName)))
	if ctype == "" {
		if mediaType == "video" {
			ctype = "video/mp4"
		} else {
			ctype = "image/jpeg"
		}
	}
	w.Header().Set("Content-Type", ctype)
	// A cached asset is identified by a content hash, so it never changes: an
	// ETag lets a client skip re-downloading it, and nosniff stops a browser
	// from reinterpreting the bytes.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if etag := cachedFileETag(info); etag != "" {
		w.Header().Set("ETag", etag)
		// http.ServeContent evaluates the conditional request against the
		// Content-Type and range handling below; ServeFile does not.
		if ifNoneMatch := strings.TrimSpace(r.Header.Get("If-None-Match")); ifNoneMatch != "" {
			for candidate := range strings.SplitSeq(ifNoneMatch, ",") {
				if strings.TrimSpace(candidate) == etag || strings.TrimSpace(candidate) == "*" {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
		}
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	http.ServeFile(w, r, fullPath)
}

// cachedFileETag is a weak validator derived from the file's identity: it is
// stable across requests and changes whenever the bytes change.
func cachedFileETag(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	return fmt.Sprintf(`W/"%x-%x"`, info.ModTime().Unix(), info.Size())
}
