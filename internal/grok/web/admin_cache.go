package web

import (
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

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

type cacheClearRequest struct {
	MediaType string `json:"media_type"`
	Type      string `json:"type"`
}

type cacheDeleteItemRequest struct {
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	FileName  string `json:"file_name"`
}

type cacheOnlineClearRequest struct {
	Token  string   `json:"token"`
	Tokens []string `json:"tokens"`
}

type cacheOnlineLoadAsyncRequest struct {
	Scope  string   `json:"scope"`
	Token  string   `json:"token"`
	Tokens []string `json:"tokens"`
}

var (
	cacheOnlineClearMu sync.Mutex
	cacheOnlineClearAt = map[string]int64{}
)

func setOnlineAssetClearTime(token string, ts int64) {
	token = NormalizeSSOToken(token)
	if token == "" || ts <= 0 {
		return
	}
	cacheOnlineClearMu.Lock()
	cacheOnlineClearAt[token] = ts
	cacheOnlineClearMu.Unlock()
}

func getOnlineAssetClearTime(token string) interface{} {
	token = NormalizeSSOToken(token)
	if token == "" {
		return nil
	}
	cacheOnlineClearMu.Lock()
	ts := cacheOnlineClearAt[token]
	cacheOnlineClearMu.Unlock()
	if ts <= 0 {
		return nil
	}
	return ts
}

func bytesToMB(size int64) float64 {
	if size <= 0 {
		return 0
	}
	return math.Round((float64(size)/1024.0/1024.0)*100) / 100
}

func cachedMediaStats(entries []cacheEntry, size int64) map[string]interface{} {
	return map[string]interface{}{
		"count":      len(entries),
		"bytes":      size,
		"size_bytes": size,
		"size_mb":    bytesToMB(size),
	}
}

func maskCacheToken(raw string) string {
	token := strings.TrimSpace(raw)
	if token == "" {
		return ""
	}
	if len(token) <= 24 {
		return token
	}
	return token[:8] + "..." + token[len(token)-16:]
}

func parsePositiveInt(raw string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

const maxCachePageSize = 1000

func resolveCacheMediaType(query url.Values) string {
	for _, key := range []string{"media_type", "type", "cache_type"} {
		v := strings.ToLower(strings.TrimSpace(query.Get(key)))
		if v != "" {
			return v
		}
	}
	return ""
}

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

func parseCacheDeleteTarget(req cacheDeleteItemRequest) (string, string, bool) {
	mediaType := strings.ToLower(firstNonEmpty(req.MediaType, req.Type))
	name := sanitizeCachedFilename(strings.TrimSpace(req.Name))
	if name == "" {
		name = sanitizeCachedFilename(strings.TrimSpace(req.FileName))
	}
	if validCacheMediaType(mediaType) && name != "" && !strings.HasPrefix(name, mediaInputFilePrefix) {
		return mediaType, name, true
	}

	rawPath := strings.TrimSpace(req.Path)
	if rawPath == "" {
		return "", "", false
	}
	if !strings.HasPrefix(rawPath, "/") {
		rawPath = "/grok/v1/files/" + strings.TrimLeft(rawPath, "/")
	}
	mt, fn, ok := parseFilesPath(rawPath)
	if !ok || strings.HasPrefix(fn, mediaInputFilePrefix) {
		return "", "", false
	}
	return mt, fn, true
}

func (h *Handler) HandleAdminCache(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	images, imageSize, err := listCachedEntries("image")
	if err != nil {
		writeGrokError(w, http.StatusInternalServerError, "failed to list cache")
		return
	}
	videos, videoSize, err := listCachedEntries("video")
	if err != nil {
		writeGrokError(w, http.StatusInternalServerError, "failed to list cache")
		return
	}

	onlineAccounts := h.listCacheOnlineAccounts(r)
	accountByToken := cacheOnlineAccountMap(onlineAccounts)

	scope := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("scope")))
	selectedToken := NormalizeSSOToken(r.URL.Query().Get("token"))
	tokensParam := strings.TrimSpace(r.URL.Query().Get("tokens"))
	selectedTokens := make([]string, 0)
	if tokensParam != "" {
		selectedTokens = normalizeOnlineTokenList(strings.Split(tokensParam, ","))
	}

	onlineScope := "none"
	switch {
	case len(selectedTokens) > 0:
		onlineScope = "selected"
	case scope == "all":
		onlineScope = "all"
	case selectedToken != "":
		onlineScope = "single"
	}

	onlineStatus := "not_loaded"
	if len(onlineAccounts) == 0 {
		onlineStatus = "no_token"
	}

	online := map[string]interface{}{
		"count":               0,
		"status":              onlineStatus,
		"token":               nil,
		"token_masked":        nil,
		"last_asset_clear_at": nil,
	}
	onlineDetails := make([]map[string]interface{}, 0)
	switch onlineScope {
	case "single":
		details, _ := h.fetchOnlineAssetDetails(r.Context(), []string{selectedToken}, accountByToken, nil)
		if len(details) > 0 {
			online["token"] = details[0]["token"]
			online["token_masked"] = details[0]["token_masked"]
			online["count"] = details[0]["count"]
			online["status"] = details[0]["status"]
			online["last_asset_clear_at"] = details[0]["last_asset_clear_at"]
		} else {
			online["token"] = selectedToken
			online["token_masked"] = maskCacheToken(selectedToken)
		}
	case "selected":
		details, total := h.fetchOnlineAssetDetails(r.Context(), selectedTokens, accountByToken, nil)
		onlineDetails = details
		online["count"] = total
		if len(selectedTokens) > 0 {
			online["status"] = "ok"
		} else {
			online["status"] = "no_token"
		}
	case "all":
		allTokens := listOnlineAccountTokens(onlineAccounts)
		details, total := h.fetchOnlineAssetDetails(r.Context(), allTokens, accountByToken, nil)
		onlineDetails = details
		online["count"] = total
		if len(allTokens) > 0 {
			online["status"] = "ok"
		} else {
			online["status"] = "no_token"
		}
	}

	imageStats := cachedMediaStats(images, imageSize)
	videoStats := cachedMediaStats(videos, videoSize)
	out := map[string]interface{}{
		"status":   "success",
		"base_dir": cacheBaseDir,
		"image":    imageStats,
		"video":    videoStats,
		"total": map[string]interface{}{
			"count": len(images) + len(videos),
			"bytes": imageSize + videoSize,
		},
		"local_image":     imageStats,
		"local_video":     videoStats,
		"online":          online,
		"online_accounts": onlineAccounts,
		"online_scope":    onlineScope,
		"online_details":  onlineDetails,
	}
	writeJSON(w, out)
}

func (h *Handler) HandleAdminCacheList(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	rawPage := strings.TrimSpace(r.URL.Query().Get("page"))
	rawPageSize := strings.TrimSpace(r.URL.Query().Get("page_size"))
	mediaType := resolveCacheMediaType(r.URL.Query())
	if mediaType == "" {
		mediaType = "image"
	}
	if !validCacheMediaType(mediaType) {
		writeGrokError(w, http.StatusBadRequest, "invalid media_type")
		return
	}
	entries, _, err := listCachedEntries(mediaType)
	if err != nil {
		writeGrokError(w, http.StatusInternalServerError, "failed to list cache")
		return
	}

	page := parsePositiveInt(rawPage, 1)
	pageSize := parsePositiveInt(rawPageSize, 1000)
	if rawPage == "" && rawPageSize == "" {
		page = 1
		pageSize = len(entries)
		if pageSize == 0 {
			pageSize = maxCachePageSize
		}
	}
	if pageSize > maxCachePageSize {
		pageSize = maxCachePageSize
	}
	paged, total := paginateCacheEntries(entries, page, pageSize)

	out := map[string]interface{}{
		"status":    "success",
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"items":     paged,
	}
	writeJSON(w, out)
}

func (h *Handler) HandleAdminCacheClear(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req cacheClearRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	mediaTypes := []string{"image"}
	targetType := strings.ToLower(firstNonEmpty(req.MediaType, req.Type))
	if targetType != "" {
		typ := targetType
		if !validCacheMediaType(typ) {
			writeGrokError(w, http.StatusBadRequest, "invalid media_type")
			return
		}
		mediaTypes = []string{typ}
	}

	removedFiles := 0
	removedBytes := int64(0)
	for _, typ := range mediaTypes {
		list, size, err := listCachedEntries(typ)
		if err != nil {
			writeGrokError(w, http.StatusInternalServerError, "failed to read cache")
			return
		}
		removedFiles += len(list)
		removedBytes += size
		for _, entry := range list {
			if err := os.Remove(filepath.Join(cacheBaseDir, typ, entry.Name)); err != nil && !os.IsNotExist(err) {
				writeGrokError(w, http.StatusInternalServerError, "failed to clear cache")
				return
			}
		}
	}

	out := map[string]interface{}{
		"status":        "success",
		"removed_count": removedFiles,
		"removed_bytes": removedBytes,
		"result": map[string]interface{}{
			"count":      removedFiles,
			"size_mb":    bytesToMB(removedBytes),
			"size_bytes": removedBytes,
		},
	}
	writeJSON(w, out)
}

func (h *Handler) HandleAdminCacheItemDelete(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req cacheDeleteItemRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	mediaType, name, ok := parseCacheDeleteTarget(req)
	if !ok {
		writeGrokError(w, http.StatusBadRequest, "invalid delete target")
		return
	}

	full := filepath.Join(cacheBaseDir, mediaType, name)
	err := os.Remove(full)
	removed := true
	if err != nil {
		if os.IsNotExist(err) {
			removed = false
		} else {
			writeGrokError(w, http.StatusInternalServerError, "failed to delete cache item")
			return
		}
	}
	out := map[string]interface{}{
		"status":     "success",
		"removed":    removed,
		"media_type": mediaType,
		"name":       name,
		"result": map[string]interface{}{
			"deleted": removed,
		},
	}
	writeJSON(w, out)
}
