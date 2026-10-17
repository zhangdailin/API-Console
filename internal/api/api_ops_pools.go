package api

import (
	"context"
	"maps"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/poolstate"
	"orchids-api/internal/util"
)

// HandleOpsPools reads pool observations without running catalog, quota or
// credential refreshes. It deliberately does not read historical aggregation.
func (a *API) HandleOpsPools(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	scope := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("channel")))
	if scope != "" && !channel.IsSupported(scope) {
		http.Error(w, "invalid channel", http.StatusBadRequest)
		return
	}
	if a.store == nil {
		http.Error(w, "account pool unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	accounts, err := a.store.ListAccounts(ctx)
	if err != nil {
		http.Error(w, "account pool unavailable", http.StatusServiceUnavailable)
		return
	}
	ids := make([]int64, 0, len(accounts))
	for _, acc := range accounts {
		if acc != nil && (scope == "" || strings.EqualFold(strings.TrimSpace(acc.AccountType), scope)) {
			ids = append(ids, acc.ID)
		}
	}
	counts := map[int64]int64{}
	checked := false
	if tracker, ok := a.connTracker.(interface {
		GetCountsChecked(context.Context, []int64) (map[int64]int64, error)
	}); ok {
		counts, err = tracker.GetCountsChecked(ctx, ids)
		if err != nil {
			http.Error(w, "account capacity unavailable", http.StatusServiceUnavailable)
			return
		}
		checked = true
	}
	now := time.Now()
	rows := make([]map[string]any, 0, 4)
	for _, def := range channel.All() {
		name := string(def.ID)
		if scope != "" && scope != name {
			continue
		}
		pool := poolstate.Counts(accounts, name, now, poolstate.NeedingLoginRefused)
		row := map[string]any{"channel": name, "accounts_enabled": pool.Enabled, "accounts_available": pool.Available, "accounts_needing_login": pool.NeedingLogin, "model_cooldowns": pool.ModelCooldowns, "capacity_available": false, "concurrency_available": checked}
		if checked {
			maps.Copy(row, opsPoolCapacity(accounts, counts, name))
			var active int64
			for _, acc := range accounts {
				if acc != nil && strings.EqualFold(strings.TrimSpace(acc.AccountType), name) {
					active += max(0, counts[acc.ID])
				}
			}
			row["active_requests"] = active
		}
		rows = append(rows, row)
	}
	w.Header().Set("Cache-Control", "no-store")
	util.WriteJSON(w, map[string]any{"available": true, "sampled_at": now.UTC().Format(time.RFC3339Nano), "capacity_scope": "account_pool", "rows": rows})
}
