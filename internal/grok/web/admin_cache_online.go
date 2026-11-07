package web

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func (h *Handler) listCacheOnlineAccounts(r *http.Request) []map[string]any {
	if h == nil || h.lb == nil || h.lb.Store == nil || r == nil {
		return []map[string]any{}
	}
	accounts, err := h.lb.Store.ListAccounts(r.Context())
	if err != nil {
		return []map[string]any{}
	}

	out := make([]map[string]any, 0, len(accounts))
	for _, acc := range CollectWebSSOSourcesByToken(accounts, true) {
		token := grokAccountToken(acc)
		out = append(out, map[string]any{
			"token":               token,
			"raw_token":           token,
			"token_masked":        maskCacheToken(token),
			"pool":                inferTokenPool(acc),
			"status":              adminTokenStatusFromAccount(acc),
			"last_asset_clear_at": getOnlineAssetClearTime(token),
		})
	}

	sort.Slice(out, func(i, j int) bool {
		left, _ := out[i]["token"].(string)
		right, _ := out[j]["token"].(string)
		return left < right
	})
	return out
}

func cacheOnlineAccountMap(onlineAccounts []map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(onlineAccounts))
	for _, item := range onlineAccounts {
		token, _ := item["token"].(string)
		token = NormalizeSSOToken(token)
		if token == "" {
			continue
		}
		out[token] = item
	}
	return out
}

func normalizeOnlineTokenList(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	seen := map[string]struct{}{}
	for _, raw := range tokens {
		token := NormalizeSSOToken(raw)
		if token == "" {
			continue
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

func onlineAccountInfo(accountByToken map[string]map[string]any, token string) (string, any) {
	masked := maskCacheToken(token)
	lastClear := getOnlineAssetClearTime(token)
	if acc, ok := accountByToken[token]; ok {
		if s, ok := acc["token_masked"].(string); ok && strings.TrimSpace(s) != "" {
			masked = s
		}
		if v, ok := acc["last_asset_clear_at"]; ok && v != nil {
			lastClear = v
		}
	}
	return masked, lastClear
}

func listOnlineAccountTokens(onlineAccounts []map[string]any) []string {
	out := make([]string, 0, len(onlineAccounts))
	for _, item := range onlineAccounts {
		token, _ := item["token"].(string)
		out = append(out, token)
	}
	return normalizeOnlineTokenList(out)
}

// runWorkerPool fans items out across up to workers goroutines, honouring ctx
// cancellation, and waits for all workers to drain before returning. Workers
// skip upstream work on cancellation; canceled optionally records each skipped item.
func runWorkerPool[T any](ctx context.Context, items []T, workers int, process func(T), canceled func(T)) {
	workers = min(workers, len(items))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range max(0, workers) {
		wg.Go(func() {
			for {
				index := int(next.Add(1) - 1)
				if index >= len(items) {
					return
				}
				if ctx.Err() != nil {
					if canceled == nil {
						return
					}
					canceled(items[index])
				} else {
					process(items[index])
				}
			}
		})
	}
	wg.Wait()
}

type indexedTokenJob struct {
	index int
	token string
}

func (h *Handler) fetchOnlineAssetDetails(
	ctx context.Context,
	tokens []string,
	accountByToken map[string]map[string]any,
	onItem func(string, map[string]any, bool),
) ([]map[string]any, int) {
	requestTokens := normalizeOnlineTokenList(tokens)
	if len(requestTokens) == 0 {
		return []map[string]any{}, 0
	}

	details := make([]map[string]any, len(requestTokens))
	if h == nil || h.webClient() == nil {
		for i, token := range requestTokens {
			masked, lastClear := onlineAccountInfo(accountByToken, token)
			details[i] = map[string]any{
				"token":               token,
				"token_masked":        masked,
				"count":               0,
				"status":              "error: client not configured",
				"last_asset_clear_at": lastClear,
			}
			if onItem != nil {
				onItem(token, details[i], false)
			}
		}
		return details, 0
	}

	var (
		totalCount int
		totalMu    sync.Mutex
	)
	jobs := make([]indexedTokenJob, len(requestTokens))
	for i, token := range requestTokens {
		jobs[i] = indexedTokenJob{index: i, token: token}
	}
	runWorkerPool(ctx, jobs, 4, func(item indexedTokenJob) {
		masked, lastClear := onlineAccountInfo(accountByToken, item.token)
		detail := map[string]any{
			"token":               item.token,
			"token_masked":        masked,
			"count":               0,
			"status":              "not_loaded",
			"last_asset_clear_at": lastClear,
		}

		count, err := h.webClient().countAssets(ctx, item.token)
		if err != nil {
			msg := strings.TrimSpace(err.Error())
			if msg == "" {
				msg = "request failed"
			}
			detail["status"] = "error: " + msg
		} else {
			detail["count"] = count
			detail["status"] = "ok"
			totalMu.Lock()
			totalCount += count
			totalMu.Unlock()
		}
		details[item.index] = detail
		if onItem != nil {
			onItem(item.token, detail, err == nil)
		}
	}, nil)
	return details, totalCount
}

func (h *Handler) HandleAdminCacheOnlineClear(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !requireGrokClient(w, h) {
		return
	}

	var req cacheOnlineClearRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	tokens, clearMode, errMsg := resolveCacheOnlineClearTargets(req, h.listCacheOnlineAccounts(r))
	if errMsg != "" {
		writeGrokError(w, http.StatusBadRequest, errMsg)
		return
	}

	results := map[string]map[string]any{}
	totalAll := 0
	successAll := 0
	failedAll := 0
	for _, token := range tokens {
		total, success, failed, err := h.webClient().clearAssets(r.Context(), token)
		if err != nil {
			results[token] = map[string]any{
				"status": "error",
				"error":  strings.TrimSpace(err.Error()),
			}
			continue
		}
		now := time.Now().UnixMilli()
		setOnlineAssetClearTime(token, now)

		totalAll += total
		successAll += success
		failedAll += failed
		results[token] = map[string]any{
			"status": "success",
			"result": map[string]any{
				"total":   total,
				"success": success,
				"failed":  failed,
			},
		}
	}

	if clearMode == "single" {
		single := results[tokens[0]]
		if status, _ := single["status"].(string); status == "success" {
			resp := map[string]any{
				"status": "success",
				"result": single["result"],
			}
			writeJSON(w, resp)
			return
		}

		errText, _ := single["error"].(string)
		errText = strings.TrimSpace(errText)
		if errText == "" {
			errText = "failed to clear cache"
		}
		resp := map[string]any{
			"status": "error",
			"error":  errText,
		}
		writeJSON(w, resp)
		return
	}

	resp := map[string]any{
		"status":  "success",
		"results": results,
		"result": map[string]any{
			"total":   totalAll,
			"success": successAll,
			"failed":  failedAll,
		},
	}

	writeJSON(w, resp)
}

func resolveCacheOnlineClearTargets(
	req cacheOnlineClearRequest,
	onlineAccounts []map[string]any,
) ([]string, string, string) {
	if req.Tokens != nil {
		tokens := normalizeOnlineTokenList(req.Tokens)
		if len(tokens) == 0 {
			return nil, "batch", "No tokens provided"
		}
		return tokens, "batch", ""
	}

	if token := NormalizeSSOToken(req.Token); token != "" {
		return []string{token}, "single", ""
	}

	allTokens := listOnlineAccountTokens(onlineAccounts)
	if len(allTokens) == 0 {
		return nil, "single", "No available token to perform cleanup"
	}
	return []string{allTokens[0]}, "single", ""
}

func (h *Handler) resolveCacheOnlineLoadTargets(
	r *http.Request,
	req cacheOnlineLoadAsyncRequest,
) ([]string, string, []map[string]any) {
	onlineAccounts := h.listCacheOnlineAccounts(r)
	scope := strings.ToLower(strings.TrimSpace(req.Scope))

	tokens := normalizeOnlineTokenList(req.Tokens)
	if len(tokens) > 0 {
		return tokens, "selected", onlineAccounts
	}
	if single := NormalizeSSOToken(req.Token); single != "" {
		return []string{single}, "single", onlineAccounts
	}
	if scope == "all" {
		return listOnlineAccountTokens(onlineAccounts), "all", onlineAccounts
	}
	return []string{}, scope, onlineAccounts
}

func (h *Handler) HandleAdminCacheOnlineLoadAsync(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !requireGrokClient(w, h) {
		return
	}

	var req cacheOnlineLoadAsyncRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	tokens, scope, onlineAccounts := h.resolveCacheOnlineLoadTargets(r, req)
	if len(tokens) == 0 {
		writeGrokError(w, http.StatusBadRequest, "no tokens provided")
		return
	}

	accountByToken := cacheOnlineAccountMap(onlineAccounts)

	ctx, cancel := context.WithCancel(context.Background())
	task := newNSFWBatchTask(len(tokens), cancel)

	go func() {
		defer scheduleDeleteNSFWBatchTask(task.ID)

		details, totalCount := h.fetchOnlineAssetDetails(ctx, tokens, accountByToken,
			func(token string, detail map[string]any, ok bool) {
				task.record(token, ok, detail)
			})

		if ctx.Err() != nil {
			task.finish("cancelled", "")
			return
		}

		images, imageSize, err := listCachedEntries("image")
		if err != nil {
			task.finish("error", "failed to list image cache")
			return
		}
		videos, videoSize, err := listCachedEntries("video")
		if err != nil {
			task.finish("error", "failed to list video cache")
			return
		}

		online := map[string]any{
			"count":               totalCount,
			"status":              "ok",
			"token":               nil,
			"token_masked":        nil,
			"last_asset_clear_at": nil,
		}
		if scope == "single" && len(details) > 0 {
			online["count"] = details[0]["count"]
			online["status"] = details[0]["status"]
			online["token"] = details[0]["token"]
			online["token_masked"] = details[0]["token_masked"]
			online["last_asset_clear_at"] = details[0]["last_asset_clear_at"]
		}

		result := map[string]any{
			"local_image":     cachedMediaStats(images, imageSize),
			"local_video":     cachedMediaStats(videos, videoSize),
			"online":          online,
			"online_accounts": onlineAccounts,
			"online_scope":    scope,
			"online_details":  details,
		}
		task.setResult(result)
		task.finish("done", "")
	}()

	writeAsyncTaskStarted(w, task)
}

func (h *Handler) HandleAdminCacheOnlineClearAsync(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !requireGrokClient(w, h) {
		return
	}

	var req cacheOnlineClearRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	tokens := make([]string, 0, len(req.Tokens)+1)
	if strings.TrimSpace(req.Token) != "" {
		tokens = append(tokens, req.Token)
	}
	tokens = append(tokens, req.Tokens...)
	tokens = normalizeOnlineTokenList(tokens)
	if len(tokens) == 0 {
		writeGrokError(w, http.StatusBadRequest, "no tokens provided")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	task := newNSFWBatchTask(len(tokens), cancel)

	go func() {
		defer scheduleDeleteNSFWBatchTask(task.ID)

		var (
			results   = map[string]map[string]any{}
			okCount   int
			failCount int
			mu        sync.Mutex
		)
		runWorkerPool(ctx, tokens, 4, func(token string) {
			total, success, failed, err := h.webClient().clearAssets(ctx, token)
			ok := err == nil
			entry := map[string]any{}
			if err != nil {
				entry["status"] = "error"
				entry["error"] = strings.TrimSpace(err.Error())
			} else {
				setOnlineAssetClearTime(token, time.Now().UnixMilli())
				entry["status"] = "success"
				entry["result"] = map[string]any{
					"total":   total,
					"success": success,
					"failed":  failed,
				}
			}

			mu.Lock()
			results[token] = entry
			if ok {
				okCount++
			} else {
				failCount++
			}
			mu.Unlock()

			task.record(token, ok, entry)
		}, nil)

		if ctx.Err() != nil {
			task.finish("cancelled", "")
			return
		}

		result := map[string]any{
			"status": "success",
			"summary": map[string]any{
				"total": len(tokens),
				"ok":    okCount,
				"fail":  failCount,
			},
			"results": results,
		}
		task.setResult(result)
		task.finish("done", "")
	}()

	writeAsyncTaskStarted(w, task)
}
