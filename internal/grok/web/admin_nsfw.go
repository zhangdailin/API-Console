package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

type adminNSFWEnableRequest struct {
	AccountIDs  []int64  `json:"account_ids"`
	Token       string   `json:"token"`
	Tokens      []string `json:"tokens"`
	Concurrency int      `json:"concurrency"`
}

type nsfwItemResult struct {
	Success     bool   `json:"success"`
	HTTPStatus  int    `json:"http_status"`
	GRPCStatus  int    `json:"grpc_status,omitempty"`
	GRPCMessage string `json:"grpc_message,omitempty"`
	Error       string `json:"error,omitempty"`
	AccountID   int64  `json:"account_id,omitempty"`
	AccountName string `json:"account_name,omitempty"`
}

type nsfwTarget struct {
	Token       string
	AccountID   int64
	AccountName string
}

type nsfwBatchTask struct {
	ID    string
	Total int

	mu      sync.RWMutex
	done    int
	ok      int
	fail    int
	status  string
	errMsg  string
	results map[string]any
	result  any

	cancel   context.CancelFunc
	doneOnce sync.Once
	doneCh   chan struct{}
}

const nsfwBatchTaskTTL = 5 * time.Minute

var (
	nsfwBatchTasksMu sync.Mutex
	nsfwBatchTasks   = map[string]*nsfwBatchTask{}
)

// maskToken names a credential without revealing any part of it: a short,
// non-reversible digest that is safe in names, responses and logs.
func maskToken(raw string) string {
	token := strings.TrimSpace(raw)
	if token == "" {
		return ""
	}
	return "sso#" + util.Fingerprint(token)
}

func grokAccountToken(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	return NormalizeSSOToken(grokSSOTokenRaw(acc))
}

func collectNSFWTargets(req adminNSFWEnableRequest, accounts []*store.Account) []nsfwTarget {
	accountByID := make(map[int64]*store.Account, len(accounts))
	for _, acc := range accounts {
		if !IsGrokWebSSOSource(acc) {
			continue
		}
		accountByID[acc.ID] = acc
	}

	dedup := map[string]nsfwTarget{}
	add := func(token string, acc *store.Account) {
		t := strings.TrimSpace(token)
		if t == "" {
			return
		}
		if _, exists := dedup[t]; exists {
			return
		}
		item := nsfwTarget{Token: t}
		if acc != nil {
			item.AccountID = acc.ID
			item.AccountName = strings.TrimSpace(acc.Name)
		}
		dedup[t] = item
	}

	// Explicit token(s) have highest priority.
	add(NormalizeSSOToken(req.Token), nil)
	for _, item := range req.Tokens {
		add(NormalizeSSOToken(item), nil)
	}

	if len(req.AccountIDs) > 0 {
		for _, id := range req.AccountIDs {
			acc := accountByID[id]
			if !IsGrokWebSSOSource(acc) {
				continue
			}
			add(grokAccountToken(acc), acc)
		}
	} else if strings.TrimSpace(req.Token) == "" && len(req.Tokens) == 0 {
		// No explicit target: default to visible Web SSO sources.
		for _, acc := range accounts {
			if !IsGrokWebSSOSource(acc) {
				continue
			}
			add(grokAccountToken(acc), acc)
		}
	}

	out := make([]nsfwTarget, 0, len(dedup))
	for _, item := range dedup {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Token < out[j].Token })
	return out
}

func normalizeNSFWConcurrency(v int) int {
	if v <= 0 {
		return 5
	}
	if v > 20 {
		return 20
	}
	return v
}

func (h *Handler) resolveNSFWTargets(r *http.Request) (adminNSFWEnableRequest, []nsfwTarget, map[string][]*store.Account, error) {
	var req adminNSFWEnableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		return req, nil, nil, fmt.Errorf("invalid json")
	}
	req.Concurrency = normalizeNSFWConcurrency(req.Concurrency)

	accounts, err := h.lb.Store.ListAccounts(r.Context())
	if err != nil {
		return req, nil, nil, fmt.Errorf("failed to list accounts")
	}
	targets := collectNSFWTargets(req, accounts)
	if len(targets) == 0 {
		return req, nil, nil, fmt.Errorf("no tokens available")
	}
	return req, targets, collectGrokAccountsByToken(accounts), nil
}

func (h *Handler) runNSFWEnableBatch(ctx context.Context, targets []nsfwTarget, tokenAccounts map[string][]*store.Account, concurrency int, onItem func(string, nsfwItemResult)) (int, map[string]nsfwItemResult) {
	var mu sync.Mutex
	okCount := 0
	results := make(map[string]nsfwItemResult, len(targets))
	process := func(target nsfwTarget) {
		res := nsfwItemResult{AccountID: target.AccountID, AccountName: target.AccountName, GRPCStatus: -1}
		if err := ctx.Err(); err != nil {
			res.Error = err.Error()
		} else {
			callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			outcome := h.webClient().EnableNSFWDetailed(callCtx, target.Token)
			res.Success, res.HTTPStatus, res.GRPCStatus = outcome.Success, outcome.HTTPStatus, outcome.GRPCStatus
			res.GRPCMessage, res.Error = outcome.GRPCMessage, strings.TrimSpace(outcome.Error)
			if !res.Success && res.Error == "" {
				res.Error = "enable nsfw failed"
			}
			if res.Success {
				for _, acc := range tokenAccounts[target.Token] {
					if acc == nil || acc.NSFWEnabled {
						continue
					}
					updated := *acc
					updated.NSFWEnabled = true
					if err := h.lb.Store.UpdateAccount(callCtx, &updated); err != nil {
						slog.Warn("persist nsfw account state failed", "account_id", acc.ID, "error", err)
						continue
					}
				}
			}
		}
		key := firstNonEmpty(maskToken(target.Token), "unknown")
		mu.Lock()
		results[key] = res
		if res.Success {
			okCount++
		}
		mu.Unlock()
		if onItem != nil {
			onItem(key, res)
		}
	}
	runWorkerPool(ctx, targets, normalizeNSFWConcurrency(concurrency), process, process)
	return okCount, results
}

func newNSFWBatchTask(total int, cancel context.CancelFunc) *nsfwBatchTask {
	id := randomHex(16)
	if id == "" {
		id = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	task := &nsfwBatchTask{
		ID:      id,
		Total:   total,
		status:  "running",
		results: map[string]any{},
		cancel:  cancel,
		doneCh:  make(chan struct{}),
	}
	nsfwBatchTasksMu.Lock()
	nsfwBatchTasks[id] = task
	nsfwBatchTasksMu.Unlock()
	return task
}

func writeAsyncTaskStarted(w http.ResponseWriter, task *nsfwBatchTask) {
	writeJSON(w, map[string]any{
		"status":  "success",
		"task_id": task.ID,
		"total":   task.Total,
	})
}

func getNSFWBatchTask(id string) (*nsfwBatchTask, bool) {
	nsfwBatchTasksMu.Lock()
	task, ok := nsfwBatchTasks[strings.TrimSpace(id)]
	nsfwBatchTasksMu.Unlock()
	return task, ok
}

func deleteNSFWBatchTask(id string) {
	nsfwBatchTasksMu.Lock()
	delete(nsfwBatchTasks, strings.TrimSpace(id))
	nsfwBatchTasksMu.Unlock()
}

func scheduleDeleteNSFWBatchTask(id string) {
	go func() {
		time.Sleep(nsfwBatchTaskTTL)
		deleteNSFWBatchTask(id)
	}()
}

func (t *nsfwBatchTask) record(masked string, ok bool, payload any) {
	if t == nil {
		return
	}
	if strings.TrimSpace(masked) == "" {
		masked = "unknown"
	}
	t.mu.Lock()
	t.results[masked] = payload
	t.done++
	if ok {
		t.ok++
	} else {
		t.fail++
	}
	t.mu.Unlock()
}

func (t *nsfwBatchTask) requestCancel() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.status == "running" {
		t.status = "cancelling"
	}
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (t *nsfwBatchTask) finish(status string, errMsg string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if strings.TrimSpace(status) == "" {
		status = "done"
	}
	t.status = status
	t.errMsg = strings.TrimSpace(errMsg)
	t.mu.Unlock()
	t.doneOnce.Do(func() { close(t.doneCh) })
}

func (t *nsfwBatchTask) setResult(payload any) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.result = payload
	t.mu.Unlock()
}

func (t *nsfwBatchTask) snapshot() map[string]any {
	if t == nil {
		return map[string]any{}
	}
	t.mu.RLock()
	results := make(map[string]any, len(t.results))
	maps.Copy(results, t.results)
	out := map[string]any{
		"task_id":   t.ID,
		"status":    t.status,
		"total":     t.Total,
		"processed": t.done,
		"done":      t.done,
		"ok":        t.ok,
		"fail":      t.fail,
		"summary": map[string]any{
			"total": t.Total,
			"done":  t.done,
			"ok":    t.ok,
			"fail":  t.fail,
		},
		"results": results,
	}
	if t.errMsg != "" {
		out["error"] = t.errMsg
	}
	if t.result != nil {
		out["result"] = t.result
	}
	t.mu.RUnlock()
	return out
}

func parseBatchTaskPath(rawPath string) (taskID string, action string, ok bool) {
	prefixes := []string{"/api/grok/admin/batch/", "/api/v1/admin/batch/", "/v1/admin/batch/"}
	prefix := ""
	for _, item := range prefixes {
		if strings.HasPrefix(rawPath, item) {
			prefix = item
			break
		}
	}
	if prefix == "" {
		return "", "", false
	}
	trim := strings.Trim(strings.TrimPrefix(rawPath, prefix), "/")
	parts := strings.Split(trim, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	taskID = strings.TrimSpace(parts[0])
	action = strings.ToLower(strings.TrimSpace(parts[1]))
	if taskID == "" {
		return "", "", false
	}
	if action != "stream" && action != "cancel" {
		return "", "", false
	}
	return taskID, action, true
}

func (h *Handler) HandleAdminNSFWEnable(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !requireGrokStore(w, h) {
		return
	}
	req, targets, tokenAccounts, err := h.resolveNSFWTargets(r)
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}

	okCount, results := h.runNSFWEnableBatch(r.Context(), targets, tokenAccounts, req.Concurrency, nil)
	out := map[string]any{
		"status": "success",
		"summary": map[string]any{
			"total": len(targets),
			"ok":    okCount,
			"fail":  len(targets) - okCount,
		},
		"results": results,
	}
	writeJSON(w, out)
}

func (h *Handler) HandleAdminNSFWEnableAsync(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !requireGrokStore(w, h) {
		return
	}
	req, targets, tokenAccounts, err := h.resolveNSFWTargets(r)
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	task := newNSFWBatchTask(len(targets), cancel)

	go func() {
		defer scheduleDeleteNSFWBatchTask(task.ID)
		_, _ = h.runNSFWEnableBatch(ctx, targets, tokenAccounts, req.Concurrency, func(masked string, res nsfwItemResult) {
			task.record(masked, res.Success, res)
		})
		if ctx.Err() != nil {
			task.finish("cancelled", "")
			return
		}
		task.finish("done", "")
	}()

	writeAsyncTaskStarted(w, task)
}

func (h *Handler) HandleAdminBatchTask(w http.ResponseWriter, r *http.Request) {
	taskID, action, ok := parseBatchTaskPath(r.URL.Path)
	if !ok {
		writeGrokError(w, http.StatusNotFound, "task not found")
		return
	}
	task, exists := getNSFWBatchTask(taskID)
	if !exists {
		writeGrokError(w, http.StatusNotFound, "task not found")
		return
	}

	switch action {
	case "cancel":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		task.requestCancel()
		out := map[string]any{
			"status": "success",
		}
		writeJSON(w, out)
		return
	case "stream":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		flusher := streamResponseHeaders(w)

		var lastPayload []byte
		send := func(payload map[string]any) bool {
			raw := encodeJSONBytes(payload)
			if bytes.Equal(raw, lastPayload) {
				return true
			}
			lastPayload = append(lastPayload[:0], raw...)
			writeSSE(w, flusher, "", raw)
			return r.Context().Err() == nil
		}

		initial := task.snapshot()
		initial["type"] = "snapshot"
		if !send(initial) {
			return
		}
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-task.doneCh:
				final := task.snapshot()
				switch strings.ToLower(strings.TrimSpace(fmt.Sprint(final["status"]))) {
				case "done":
					final["type"] = "done"
				case "cancelled":
					final["type"] = "cancelled"
				case "error":
					final["type"] = "error"
				default:
					final["type"] = "done"
				}
				_ = send(final)
				return
			case <-ticker.C:
				tick := task.snapshot()
				tick["type"] = "snapshot"
				_ = send(tick)
			}
		}
	default:
		writeGrokError(w, http.StatusNotFound, "task not found")
		return
	}
}

func collectGrokAccountsByToken(accounts []*store.Account) map[string][]*store.Account {
	result := make(map[string][]*store.Account, len(accounts))
	for token, acc := range CollectWebSSOSourcesByToken(accounts, false) {
		result[token] = []*store.Account{acc}
	}
	return result
}
