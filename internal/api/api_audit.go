package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/util"
)

// auditScanCap bounds the bounded ledger scans used by operations coverage and
// account usage estimates.
const auditScanCap = 2000

// writeAccountCheckBusy tells the caller that a refresh of this account is already
// running, so the click was merged instead of racing a second refresh.
func writeAccountCheckBusy(w http.ResponseWriter) {
	util.WriteJSONStatus(w, http.StatusConflict, map[string]interface{}{
		"error": map[string]interface{}{
			"type":    "check_in_progress",
			"message": "this account is already being refreshed; the request was merged",
		},
	})
}

// auditQueryFilter is the log centre's filter set.
type auditQueryFilter struct {
	kind      string
	channel   string
	status    string
	action    string
	actor     string
	model     string
	outcome   string
	since     time.Time
	until     time.Time
	accountID int64
	apiKeyID  int64
}

// auditOutcomeLabels names the result classes the operations overview counts, so a
// drill-down chip and the chart that opened it use the same words. The ids are the
// ones the log centre's select offers.
var auditOutcomeLabels = map[string]string{
	"failed":          "失败（全部失败类型）",
	"success":         "成功",
	"rate_limited":    "限流 429/529",
	"client_error":    "客户端错误 4xx",
	"server_error":    "上游错误 5xx",
	"stream_error":    "流中断",
	"upstream_auth":   "上游认证失败 401/403",
	"rejected":        "网关拒绝 401/403",
	"quota_exhausted": "额度用尽 402",
}

// auditTimeFormats are the spellings a time filter accepts. The log centre's form
// asks for RFC3339, but an operator copying a timestamp out of a log, or a
// bookmark written by hand, must not silently lose the filter.
var auditTimeFormats = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parseAuditTime reads one boundary of the time range. An empty value means "not
// filtered" and comes back as a zero time; an unparseable value is an error, because
// ignoring it is indistinguishable from a filter that does not work.
func parseAuditTime(name, raw string) (time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range auditTimeFormats {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	// Epoch seconds or milliseconds: what a script or an export hands over.
	if epoch, err := strconv.ParseInt(value, 10, 64); err == nil {
		switch {
		case epoch > 1e12:
			return time.UnixMilli(epoch), nil
		case epoch > 1e9:
			return time.Unix(epoch, 0), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid %s: %q (expected RFC3339, e.g. 2026-09-13T00:00:00Z)", name, value)
}

func auditFilterFromQuery(r *http.Request) (auditQueryFilter, error) {
	query := r.URL.Query()
	parse := func(name string) int64 {
		parsed, err := strconv.ParseInt(strings.TrimSpace(query.Get(name)), 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	}
	since, err := parseAuditTime("since", query.Get("since"))
	if err != nil {
		return auditQueryFilter{}, err
	}
	until, err := parseAuditTime("until", query.Get("until"))
	if err != nil {
		return auditQueryFilter{}, err
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		return auditQueryFilter{}, fmt.Errorf("until %s is before since %s", until.Format(time.RFC3339), since.Format(time.RFC3339))
	}
	return auditQueryFilter{
		kind:      strings.ToLower(strings.TrimSpace(query.Get("kind"))),
		channel:   strings.ToLower(strings.TrimSpace(query.Get("channel"))),
		status:    strings.ToLower(strings.TrimSpace(query.Get("status"))),
		action:    strings.ToLower(strings.TrimSpace(query.Get("action"))),
		actor:     strings.ToLower(strings.TrimSpace(query.Get("actor"))),
		model:     strings.ToLower(strings.TrimSpace(query.Get("model"))),
		outcome:   strings.ToLower(strings.TrimSpace(query.Get("outcome"))),
		since:     since,
		until:     until,
		accountID: parse("account_id"),
		apiKeyID:  parse("api_key_id"),
	}, nil
}

// parseAuditLimit reads the journal's `limit` query, clamped to the range one
// page may ask for; an unparsable value keeps the default.
func parseAuditLimit(r *http.Request) int {
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	return limit
}

// journalMaxID resolves where a reverse scan starts: an explicit `before` cursor
// wins, otherwise a window that ends in the past starts inside itself, since
// stream ids are time-ordered and anything newer would only be filtered out.
func journalMaxID(r *http.Request, filter auditQueryFilter) string {
	if before := strings.TrimSpace(r.URL.Query().Get("before")); before != "" {
		return "(" + before
	}
	if !filter.until.IsZero() {
		return "(" + strconv.FormatInt(filter.until.UnixMilli()+1, 10)
	}
	return "+"
}

// auditOutcomeClass names a record's result the way the operations overview counts
// a request (see opsagg's observeDetails), so "result = rate limited" in the log centre
// lists the requests the chart counted as rate limited. An operation has no HTTP class:
// it either happened or it did not.
func auditOutcomeClass(event audit.Event) string {
	if isRequestQualityEvent(event) {
		return ""
	}
	status := strings.ToLower(strings.TrimSpace(event.Status))
	if event.Kind != audit.KindRequest {
		switch status {
		case "":
			return ""
		case "ok", "success", "recovered", "cleared":
			// A recovery is good news: an alert that stopped firing, or a
			// credential a healthy turn released from its quality cooldown.
			return "success"
		default:
			return "failed"
		}
	}
	httpStatus := int(metadataInt(event.Metadata, "http_status"))
	switch status {
	case "success", "ok", "stop", "tool_calls", "length", "content_filter", "recovered":
		return "success"
	case "stream_error":
		// The status line was already committed: a class of its own, exactly as the
		// overview records it.
		return "stream_error"
	}
	if status == "" {
		// Older records have no status column; the HTTP status is all there is.
		if httpStatus >= 200 && httpStatus < 300 {
			return "success"
		}
		if httpStatus == 0 {
			return ""
		}
	}
	// Provider finish reasons evolve. A completed 2xx response is successful
	// unless the journal explicitly recorded an error; retain the raw finish
	// reason for the UI's more specific badge/tooltip.
	if httpStatus >= 200 && httpStatus < 300 && status != "error" && status != "failed" {
		return "success"
	}
	switch {
	case httpStatus == 429 || httpStatus == 529:
		return "rate_limited"
	case httpStatus == 402:
		return "quota_exhausted"
	case httpStatus == 401 || httpStatus == 403:
		// A refusal on a provider channel is the provider rejecting our credential;
		// a record with no channel never reached a provider, so the gate refused the
		// caller. The overview separates the two the same way.
		if strings.TrimSpace(event.Channel) == "" {
			return "rejected"
		}
		return "upstream_auth"
	case httpStatus >= 500:
		return "server_error"
	case httpStatus >= 400:
		return "client_error"
	}
	// A failure with no HTTP class (a transport error before any status line).
	return "failed"
}

func auditOutcomeLabel(class string) string {
	if label, ok := auditOutcomeLabels[class]; ok {
		return label
	}
	return class
}

func (f auditQueryFilter) matches(event audit.Event) bool {
	if f.kind != "" && string(event.Kind) != f.kind || f.channel != "" && !strings.EqualFold(event.Channel, f.channel) {
		return false
	}
	if f.status != "" && !strings.EqualFold(event.Status, f.status) || f.action != "" && !strings.Contains(strings.ToLower(event.Action), f.action) {
		return false
	}
	if f.actor != "" && !strings.Contains(strings.ToLower(event.Actor), f.actor) || f.model != "" && !strings.Contains(strings.ToLower(event.Model), f.model) {
		return false
	}
	// The time range is the filter a drill-down always carries: the overview counted
	// one window, so a list that ignores it shows records the chart never saw.
	if !f.since.IsZero() && event.Timestamp.Before(f.since) || !f.until.IsZero() && event.Timestamp.After(f.until) {
		return false
	}
	if f.outcome != "" {
		class := auditOutcomeClass(event)
		if f.outcome == "failed" {
			if class == "" || class == "success" {
				return false
			}
		} else if class != f.outcome {
			return false
		}
	}
	return !(f.accountID != 0 && event.AccountID != f.accountID || f.apiKeyID != 0 && event.APIKeyID != f.apiKeyID)
}

func (f auditQueryFilter) describe() map[string]interface{} {
	described := map[string]interface{}{}
	if f.kind != "" {
		described["kind"] = f.kind
	}
	if f.channel != "" {
		described["channel"] = f.channel
	}
	if f.status != "" {
		described["status"] = f.status
	}
	if f.action != "" {
		described["action"] = f.action
	}
	if f.actor != "" {
		described["actor"] = f.actor
	}
	if f.model != "" {
		described["model"] = f.model
	}
	if f.outcome != "" {
		described["outcome"] = f.outcome
		described["outcome_label"] = auditOutcomeLabel(f.outcome)
	}
	if !f.since.IsZero() {
		described["since"] = f.since.UTC().Format(time.RFC3339)
	}
	if !f.until.IsZero() {
		described["until"] = f.until.UTC().Format(time.RFC3339)
	}
	if f.accountID != 0 {
		described["account_id"] = f.accountID
	}
	if f.apiKeyID != 0 {
		described["api_key_id"] = f.apiKeyID
	}
	return described
}

// auditCoverage reports the retained window and the per-journal counts, so the
// UI can state what the numbers actually cover instead of promising a fixed
// retention period.
func (a *API) auditCoverage(ctx context.Context) map[string]interface{} {
	coverage := map[string]interface{}{"available": false, "entries": 0, "oldest": nil, "newest": nil, "counts": map[string]int{}}
	if a == nil || a.store == nil || a.store.RedisClient() == nil {
		return coverage
	}
	client := a.store.RedisClient()
	key := a.store.RedisPrefix() + "audit:log"

	total, err := client.XLen(ctx, key).Result()
	if err != nil {
		return coverage
	}
	coverage["available"] = true
	coverage["entries"] = total
	counts := map[string]int{}
	var newest, oldest string
	if entries, err := client.XRevRangeN(ctx, key, "+", "-", 1).Result(); err == nil && len(entries) > 0 {
		newest = entries[0].ID
	}
	if entries, err := client.XRangeN(ctx, key, "-", "+", 1).Result(); err == nil && len(entries) > 0 {
		oldest = entries[0].ID
	}
	coverage["oldest"] = streamIDTime(oldest)
	coverage["newest"] = streamIDTime(newest)

	// Per-journal counts over the retained window. The cap keeps the call bounded
	// on a busy instance; counts are therefore a floor, which the response says.
	if entries, err := client.XRevRangeN(ctx, key, "+", "-", auditScanCap).Result(); err == nil {
		coverage["count_sampled"] = len(entries)
		for _, entry := range entries {
			raw, _ := entry.Values["data"].(string)
			var event audit.Event
			if raw == "" || json.Unmarshal([]byte(raw), &event) != nil {
				continue
			}
			kind := string(event.Kind)
			if kind == "" {
				kind = string(audit.KindRequest)
			}
			counts[kind]++
		}
	}
	coverage["counts"] = counts
	return coverage
}

// streamIDTime converts a Redis Stream ID ("ms-seq") into RFC3339, or null.
func streamIDTime(id string) interface{} {
	millis := strings.SplitN(strings.TrimSpace(id), "-", 2)[0]
	if millis == "" {
		return nil
	}
	parsed, err := strconv.ParseInt(millis, 10, 64)
	if err != nil || parsed <= 0 {
		return nil
	}
	return time.UnixMilli(parsed).UTC().Format(time.RFC3339)
}
