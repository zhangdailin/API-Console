package web

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// cacheEntry is one generated image or video stored in the local media cache
// (cacheBaseDir/<image|video>). The media library lists them.
type cacheEntry struct {
	MediaType  string `json:"media_type"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	URL        string `json:"url"`
	ViewURL    string `json:"view_url"`
	PreviewURL string `json:"preview_url,omitempty"`
	Size       int64  `json:"size"`
	SizeBytes  int64  `json:"size_bytes"`
	UpdatedAt  int64  `json:"updated_at"`
	MtimeMS    int64  `json:"mtime_ms"`
}

func bytesToMB(size int64) float64 {
	if size <= 0 {
		return 0
	}
	return math.Round((float64(size)/1024.0/1024.0)*100) / 100
}

func parsePositiveInt(raw string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

const maxCachePageSize = 1000

func paginateCacheEntries(entries []cacheEntry, page int, pageSize int) ([]cacheEntry, int) {
	total := len(entries)
	if total == 0 {
		return []cacheEntry{}, 0
	}
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = total
	}
	if pageSize > maxCachePageSize {
		pageSize = maxCachePageSize
	}
	// Avoid (page-1)*pageSize overflow for attacker-controlled page values.
	if page > (total-1)/pageSize+1 {
		return []cacheEntry{}, total
	}
	start := (page - 1) * pageSize
	if start >= total {
		return []cacheEntry{}, total
	}
	end := start + pageSize
	if end < start || end > total {
		end = total
	}
	return entries[start:end], total
}

func validCacheMediaType(mediaType string) bool {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return mediaType == "image" || mediaType == "video"
}

func listCachedEntries(mediaType string) ([]cacheEntry, int64, error) {
	typ := strings.ToLower(strings.TrimSpace(mediaType))
	if !validCacheMediaType(typ) {
		return nil, 0, nil
	}
	dir := filepath.Join(cacheBaseDir, typ)
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []cacheEntry{}, 0, nil
		}
		return nil, 0, err
	}

	out := make([]cacheEntry, 0, len(items))
	var totalSize int64
	for _, item := range items {
		if !item.Type().IsRegular() {
			continue
		}
		name := sanitizeCachedFilename(item.Name())
		if name == "" || strings.HasPrefix(name, mediaInputFilePrefix) {
			continue
		}
		info, err := item.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		totalSize += info.Size()
		viewURL := "/v1/files/" + typ + "/" + name
		previewURL := ""
		if typ == "image" {
			previewURL = viewURL
		}
		out = append(out, cacheEntry{
			MediaType:  typ,
			Name:       name,
			Path:       typ + "/" + name,
			URL:        "/grok/v1/files/" + typ + "/" + name,
			ViewURL:    viewURL,
			PreviewURL: previewURL,
			Size:       info.Size(),
			SizeBytes:  info.Size(),
			UpdatedAt:  info.ModTime().UnixMilli(),
			MtimeMS:    info.ModTime().UnixMilli(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, totalSize, nil
}
