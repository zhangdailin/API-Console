package api

import (
	"context"
	"maps"
	"net/http"
	"orchids-api/internal/poolstate"
	"orchids-api/internal/util"
	"sort"
	"strconv"
	"strings"
	"time"

	"encoding/json"

	"github.com/redis/go-redis/v9"

	"orchids-api/internal/alerting"
	"orchids-api/internal/audit"
	"orchids-api/internal/opsagg"
	"orchids-api/internal/pricing"
)

// defaultOpsWindowMinutes is the window the overview opens with.
const defaultOpsWindowMinutes = 180

const alertRulesRedisKey = "ops:alert_rules"

// nonProviderChannels are aggregates that are counted but must not be presented
// as provider channels in the matrix or the channel picker:
//
// "http" is every request whose path matched no provider prefix: the admin UI,
// health checks, and whatever a public scanner asks for. It has no accounts and
// no models, so it cannot be "healthy" or "unhealthy".
//
// "probe" stays listed for legacy Redis aggregates written by older builds that
// ran the removed synthetic probe loop; no new probe rows are ever recorded.
var nonProviderChannels = map[string]bool{
	"http":  true,
	"probe": true,
}

// IsProviderChannel reports whether a channel is one an operator can add an
// account to and route a model through.
func IsProviderChannel(channel string) bool {
	name := strings.ToLower(strings.TrimSpace(channel))
	if name == "" {
		return false
	}
	return !nonProviderChannels[name]
}

// alertingSuccessTargetSource names where the target comes from. The page shows
// it verbatim next to the percentage, so it doubles as the provenance: the number
// is a policy threshold, not a measured average of recent traffic.
const alertingSuccessTargetSource = "告警阈值 SuccessRateWarning"

// successTarget answers the question the operations page could not: what is the
// displayed success rate measured against? The threshold is read from the engine
// rather than hard-coded so the number on the page and the number the alerts fire
// on cannot drift apart.
//
// A nil engine — aggregation disabled, a test, or a deployment that never wired
// alerting — still has to answer, and so does an engine built from a zero-value
// Rules: the shipped policy is the honest fallback, because a returned 0 would
// make the page print "target 0.0%" and look broken.
func successTarget(engine *alerting.Engine) (float64, string) {
	return successTargets(engine).SuccessRateWarning, alertingSuccessTargetSource
}

// successTargetCritical is the severe line: below it a channel is broken whatever
// the failure count. It rides along because the alert text quotes the very same
// number, and one source is better than two that can disagree.
func successTargetCritical(engine *alerting.Engine) float64 {
	return successTargets(engine).SuccessRateCritical
}

// successTargets reads both success-rate thresholds, falling back to the shipped
// policy wherever the engine carries no usable line. A nil engine — aggregation
// disabled, a test, or a deployment that never wired alerting — still has to
// answer, and so does an engine built from a zero-value Rules: a returned 0
// would make the page print "target 0.0%" and look broken.
func successTargets(engine *alerting.Engine) alerting.Rules {
	rules := engine.Thresholds()
	defaults := alerting.DefaultRules()
	if rules.SuccessRateWarning <= 0 {
		rules.SuccessRateWarning = defaults.SuccessRateWarning
	}
	if rules.SuccessRateCritical <= 0 {
		rules.SuccessRateCritical = defaults.SuccessRateCritical
	}
	return rules
}

// HandleOpsOverview answers the operations overview: KPI totals, a per-minute
// trend and the current alert set. Every number states its sample count, so the
// UI can show "no samples yet" instead of a healthy-looking zero.
func (a *API) HandleOpsOverview(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	window, since, until := a.parseOpsWindow(r)
	channels, aggregates, channelErr := a.opsChannels(r.Context(), since, until)
	target := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("channel")))

	payload := map[string]interface{}{
		"window_minutes": window,
		"since":          since.UTC().Format(time.RFC3339),
		"until":          until.UTC().Format(time.RFC3339),
		"channels":       channels,
		// Named so the page can explain why some traffic is counted but not shown
		// as a channel (the http catch-all, plus legacy probe aggregates from
		// builds that still ran the removed synthetic probe loop).
		"excluded_aggregates": aggregates,
		"channel":             target,
		"totals":              opsagg.Summary{},
		"series":              []map[string]interface{}{},
		"alerts":              []alerting.Alert{},
		"coverage":            a.auditCoverage(r.Context()),
		"aggregation":         "per-minute",
		"retention_hours":     int(opsagg.BucketRetention.Hours()),
	}

	// The target the success rate is judged against, set immediately after the
	// literal so the early return below (aggregation unavailable) carries it too:
	// that branch still renders a success-rate KPI, and a percentage with no
	// stated target is exactly the confusion this field removes.
	//
	// success_target_critical is the severe line the alert detail already quotes
	// ("threshold <50%"); the page does not draw it yet, but keeping it in the same
	// response means the two numbers an operator compares have one source.
	successTargetValue, targetSource := successTarget(a.alertEngine)
	if a.auditHealth != nil {
		payload["ingestion"] = a.auditHealth()
	}
	payload["aggregation_health"] = a.opsAggregator.Health()
	payload["latency_precision"] = "每个渠道分钟桶最多保留最近 5000 个延迟样本，模型分钟桶最多 2000 个；P95 为保留样本的最近秩分位数，窗口按整分钟边界统计。"
	payload["success_target"] = successTargetValue
	payload["success_target_source"] = targetSource
	payload["success_target_critical"] = successTargetCritical(a.alertEngine)

	if a.opsAggregator == nil || !a.opsAggregator.Enabled() {
		payload["available"] = false
		payload["note"] = "指标聚合需要 Redis；当前部署未启用。"
		util.WriteJSON(w, payload)
		return
	}
	if channelErr != nil {
		http.Error(w, "failed to discover metric channels", http.StatusServiceUnavailable)
		return
	}
	payload["available"] = true

	scope := target
	scope = util.FirstNonEmptyUntrimmed(scope, "all")
	buckets, durations, ttfts, err := a.opsBucketsWithSamples(r.Context(), scope, since, until)
	if err != nil {
		http.Error(w, "failed to read metric buckets", http.StatusInternalServerError)
		return
	}
	payload["totals"] = a.opsAggregator.SummarizeWith(r.Context(), opsagg.SummaryInput{
		Channel:       scope,
		Buckets:       buckets,
		WindowMinutes: float64(window),
		// The samples were collected here, including the merged case, so the
		// aggregator must not go looking for a per-scope list that does not exist.
		Durations:       durations,
		FirstTokenMS:    ttfts,
		SamplesProvided: true,
	})
	payload["series"] = opsSeries(buckets)
	matrix, err := a.opsMatrix(r.Context(), channels, since, until)
	if err != nil {
		http.Error(w, "failed to read channel metrics", http.StatusServiceUnavailable)
		return
	}
	payload["matrix"] = matrix
	payload["alerts"] = a.firingAlerts()
	payload["concurrency"] = a.currentConcurrency()
	if err := ctx.Err(); err != nil {
		http.Error(w, "monitoring query timed out", http.StatusServiceUnavailable)
		return
	}

	util.WriteJSON(w, payload)
}

// HandleOpsAlertRules exposes the exact policy used by the alert engine. Saved
// rules are persisted in Redis and take effect on the next evaluation tick.
func (a *API) HandleOpsAlertRules(w http.ResponseWriter, r *http.Request) {
	defaults := alerting.DefaultRules()
	if a == nil || a.alertEngine == nil || a.store == nil || a.store.RedisClient() == nil {
		util.WriteJSON(w, map[string]interface{}{
			"rules": defaults, "defaults": defaults, "editable": false,
			"note": "告警规则需要 Redis 和告警引擎。",
		})
		return
	}
	if r.Method == http.MethodGet {
		util.WriteJSON(w, map[string]interface{}{
			"rules": a.alertEngine.Thresholds(), "defaults": defaults, "editable": true,
		})
		return
	}
	if r.Method != http.MethodPut {
		writeMethodNotAllowed(w)
		return
	}
	var rules alerting.Rules
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := decoder.Decode(&rules); err != nil {
		http.Error(w, "Invalid alert rules", http.StatusBadRequest)
		return
	}
	if err := rules.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.alertRulesMu.Lock()
	defer a.alertRulesMu.Unlock()
	raw, _ := json.Marshal(rules)
	if err := a.store.RedisClient().Set(r.Context(), a.store.RedisPrefix()+alertRulesRedisKey, raw, 0).Err(); err != nil {
		http.Error(w, "Could not persist alert rules", http.StatusInternalServerError)
		return
	}
	_ = a.alertEngine.SetThresholds(rules)
	util.WriteJSON(w, map[string]interface{}{"rules": rules, "defaults": defaults, "editable": true})
}

// journalAttemptLookback bounds the extra scan that recovers upstream attempts whose
// request sits at the edge of a page window. Attempts are written before the request,
// so they can sit arbitrarily far behind it when other traffic is journalled
// concurrently; the bound keeps a page load bounded, at the price of an honestly
// incomplete detail panel for a request that waited behind more entries than this.
const journalAttemptLookback = 1000

// HandleJournalRecords answers one journal tab, filtered by kind and joined
// with the upstream attempts its request produced.
func (a *API) HandleJournalRecords(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.writeJournalList(w, r)
	case http.MethodPost:
		// Reserved for future journal actions (acknowledge an alert, annotate an
		// operation). Answering explicitly beats a confusing 405.
		http.Error(w, "no journal action is defined", http.StatusNotImplemented)
	default:
		writeMethodNotAllowed(w)
	}
}

// pricingBreakdownForJournal rebuilds the rate components of a priced row. The
// quantities come from the row's own usage counters plus the media counts the
// row recorded in its metadata; a row without a pricing model, or whose
// quantities are missing, simply has no breakdown.
func pricingBreakdownForJournal(event audit.Event) (pricing.Breakdown, bool) {
	if strings.TrimSpace(event.PricingModel) == "" || event.CostInUSDTicks <= 0 {
		return pricing.Breakdown{}, false
	}
	quantities := pricing.Quantities{
		InputTokens:  int64(event.InputTokens),
		CachedTokens: int64(event.CachedInputTokens),
		OutputTokens: int64(event.OutputTokens),
	}
	if event.Metadata != nil {
		quantities.OutputImages = metadataInt(event.Metadata, "images")
		quantities.InputImages = metadataInt(event.Metadata, "input_images")
		quantities.OutputSeconds = metadataInt(event.Metadata, "seconds")
		quantities.Characters = metadataInt(event.Metadata, "characters")
		if duration, ok := event.Metadata["duration_seconds"].(float64); ok {
			quantities.StreamingSeconds = duration
			quantities.OutputSeconds = int64(duration)
		}
	}
	breakdown, ok := pricing.ReconstructBreakdown(event.PricingModel, quantities)
	if !ok {
		return pricing.Breakdown{}, false
	}
	return breakdown, true
}

// metadataInt reads an integer the journal kept under metadata. The request
// record's HTTP status, the media counts a row priced, and the first-token
// latency live there rather than in a column of their own, and they arrive as
// whatever the encoder on the other side produced: a JSON number, a Go integer,
// or a stringified one. One reader for all of them keeps the log centre's
// outcome class and the journal's cost breakdown in agreement about what a row
// said.
func metadataInt(metadata map[string]interface{}, key string) int64 {
	switch value := metadata[key].(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case float64:
		return int64(value)
	case json.Number:
		parsed, _ := value.Int64()
		return parsed
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(value))
		return int64(parsed)
	default:
		return 0
	}
}

func (a *API) writeJournalList(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.store == nil || a.store.RedisClient() == nil {
		http.Error(w, "journal requires Redis storage", http.StatusServiceUnavailable)
		return
	}
	limit := parseAuditLimit(r)
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	if kind == "" {
		kind = string(audit.KindRequest)
	}
	filter, err := auditFilterFromQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter.kind = kind

	maxID := journalMaxID(r, filter)

	client := a.store.RedisClient()
	key := a.store.RedisPrefix() + "audit:log"
	scanCap := int64(limit) * 6
	entries, err := client.XRevRangeN(r.Context(), key, maxID, "-", scanCap).Result()
	if err != nil {
		http.Error(w, "failed to read journal", http.StatusInternalServerError)
		return
	}

	// Two passes over the same window, because the stream is strictly
	// newest-first: a request is written when it finishes and its upstream
	// attempts were written before it, so in reverse order the attempts always
	// appear AFTER the request that owns them. Attaching them during the scan
	// therefore left every detail panel empty.
	//
	// First pass: collect every upstream attempt in the window, keyed by request.
	// Second pass: emit the matching records with their attempts already known.
	// The quality checks of a request are collected the same way: each withheld
	// attempt and the conclusion are rows of their own, written before the
	// request row that owns them.
	attempts := map[string][]audit.Event{}
	quality := map[string][]audit.Event{}
	for _, entry := range entries {
		event, ok := decodeAuditEvent(entry)
		if !ok || event.RequestID == "" {
			continue
		}
		switch {
		case event.Action == "grok_upstream_attempt":
			attempts[event.RequestID] = append(attempts[event.RequestID], event)
		case isRequestQualityEvent(event):
			quality[event.RequestID] = append(quality[event.RequestID], event)
		}
	}

	records := make([]map[string]interface{}, 0, limit)
	matched := 0
	for _, entry := range entries {
		event, ok := decodeAuditEvent(entry)
		if !ok {
			continue
		}
		if event.Action == "grok_upstream_attempt" {
			continue
		}
		if !filter.matches(event) {
			continue
		}
		matched++
		records = append(records, map[string]interface{}{
			"id":       entry.ID,
			"event":    event,
			"attempts": attempts[event.RequestID],
		})
		if checks := quality[event.RequestID]; len(checks) > 0 && event.Kind == audit.KindRequest {
			records[len(records)-1]["quality"] = chronological(checks)
		}
		if breakdown, priced := pricingBreakdownForJournal(event); priced {
			// The row keeps the cost and the pricing model; the components behind
			// them are reconstructed on read so an operator can see which rate
			// produced the number instead of taking it on faith.
			records[len(records)-1]["pricing_breakdown"] = breakdown
		}
		if class := auditOutcomeClass(event); class != "" {
			// The result class travels with the record: it is what the row's badge and
			// the drill-down that opened the list both talk about.
			records[len(records)-1]["outcome_class"] = class
			records[len(records)-1]["outcome_label"] = auditOutcomeLabel(class)
		}
		if len(records) >= limit {
			break
		}
	}

	// Attempts are written BEFORE their request, so the attempts of the oldest record
	// on the page can sit behind the page window: a request that ran while other
	// traffic was journalled would come back without its detail panel. One extra
	// bounded scan, started just past everything already read, recovers them. It
	// never moves the cursor, so nothing it reads is skipped by the next page.
	if len(records) > 0 && len(entries) > 0 {
		missing := map[string]bool{}
		// A conclusion can be in the page while earlier withheld attempts are not.
		missingQuality := map[string]bool{}
		for _, record := range records {
			event := record["event"].(audit.Event)
			if event.RequestID == "" {
				continue
			}
			if len(attempts[event.RequestID]) == 0 {
				missing[event.RequestID] = true
			}
			if event.Kind == audit.KindRequest && !isRequestQualityEvent(event) && (qualityResolutionOf(event) != "" || metadataInt(event.Metadata, "quality_checked_attempts") > 0) && qualityChecksIncomplete(event, quality[event.RequestID]) {
				missingQuality[event.RequestID] = true
			}
		}
		if len(missing) > 0 || len(missingQuality) > 0 {
			older, readErr := client.XRevRangeN(r.Context(), key, "("+entries[len(entries)-1].ID, "-", journalAttemptLookback).Result()
			if readErr == nil {
				for _, entry := range older {
					event, ok := decodeAuditEvent(entry)
					if !ok {
						continue
					}
					switch {
					case event.Action == "grok_upstream_attempt" && missing[event.RequestID]:
						attempts[event.RequestID] = append(attempts[event.RequestID], event)
					case isRequestQualityEvent(event) && missingQuality[event.RequestID]:
						quality[event.RequestID] = append(quality[event.RequestID], event)
					}
				}
				for _, record := range records {
					event := record["event"].(audit.Event)
					if list := attempts[event.RequestID]; len(list) > 0 {
						record["attempts"] = list
					}
					if checks := quality[event.RequestID]; len(checks) > 0 && missingQuality[event.RequestID] {
						record["quality"] = chronological(checks)
					}
				}
			}
		}
	}

	ids := make([]string, 0, len(records))
	for _, record := range records {
		event := record["event"].(audit.Event)
		if event.RequestID != "" {
			ids = append(ids, event.RequestID)
		}
	}
	indexes, err := a.diagnostics.Indexes(r.Context(), ids)
	if err != nil {
		http.Error(w, "Could not read diagnostic indexes", http.StatusServiceUnavailable)
		return
	}
	for _, record := range records {
		event := record["event"].(audit.Event)
		if index, ok := indexes[event.RequestID]; ok {
			record["diagnostics"] = index
		}
	}

	// The cursor must never skip a record. A FULL page resumes strictly before the
	// last row shown: everything newer was scanned, so no match is left behind — and
	// pointing the cursor at the end of the scan window instead (as this used to) threw
	// away every match between the page's last row and the window's edge, which is why
	// 600 stored requests only ever yielded 100 in the list. A SHORT page resumes past
	// everything it scanned, because the scan budget ran out before the ledger did;
	// that keeps the entries behind it reachable, which is the "earlier operation logs
	// cannot be found" case. An empty cursor means the ledger itself ended.
	nextCursor := ""
	switch {
	case len(records) >= limit:
		nextCursor = records[len(records)-1]["id"].(string)
	case int64(len(entries)) >= scanCap && len(entries) > 0:
		nextCursor = entries[len(entries)-1].ID
	}
	util.WriteJSON(w, map[string]interface{}{
		"data":        records,
		"next_cursor": nextCursor,
		"kind":        kind,
		"scanned":     len(entries),
		"matched":     matched,
		"scan_cap":    scanCap,
		// What the filters actually did, so the page can name the result class it was
		// narrowed to instead of leaving the reader to guess.
		"filter_used": filter.describe(),
		"coverage":    a.auditCoverage(r.Context()),
	})
}

// Quality stages remain visible as rows even before the request finishes, and
// are also joined into its chronological timeline. Credential verdicts stay in
// the system journal.
func isRequestQualityEvent(event audit.Event) bool {
	switch event.Action {
	case "grok_quality_check", "grok_quality_retry", "grok_quality_degraded", "grok_quality_resolution":
		return true
	}
	return false
}

func qualityChecksIncomplete(event audit.Event, checks []audit.Event) bool {
	var withheld, started, finished int64
	concluded := false
	for _, check := range checks {
		switch check.Action {
		case "grok_quality_degraded":
			withheld++
			finished++
		case "grok_quality_check":
			if check.Status == "checking" {
				started++
			} else {
				finished++
			}
		case "grok_quality_resolution":
			concluded = true
		}
	}
	expected := metadataInt(event.Metadata, "quality_checked_attempts")
	return qualityResolutionOf(event) != "" && !concluded || withheld < metadataInt(event.Metadata, "quality_withheld_attempts") || started < expected || finished < expected
}

// qualityResolutionOf reads how a request that had a turn withheld ended, from
// the request's own row. It is empty for a request no check ever refused.
func qualityResolutionOf(event audit.Event) string {
	resolution, _ := event.Metadata["quality_resolution"].(string)
	return strings.ToLower(strings.TrimSpace(resolution))
}

// chronological returns journal rows oldest first. The scan reads the stream
// newest first, and a detail panel tells a request's story in the order it
// happened.
func chronological(events []audit.Event) []audit.Event {
	out := make([]audit.Event, len(events))
	for i, event := range events {
		out[len(events)-1-i] = event
	}
	return out
}

func decodeAuditEvent(entry redis.XMessage) (audit.Event, bool) {
	raw, _ := entry.Values["data"].(string)
	if raw == "" {
		return audit.Event{}, false
	}
	var event audit.Event
	if json.Unmarshal([]byte(raw), &event) != nil {
		return audit.Event{}, false
	}
	if event.Kind == "" {
		// Records written before the journal was split have no kind; inference
		// traffic used the request actions.
		switch {
		case strings.HasSuffix(event.Action, "_request"), event.Action == "grok_upstream_attempt", event.Action == "http_request":
			event.Kind = audit.KindRequest
		default:
			event.Kind = audit.KindOperation
		}
	}
	return event, true
}

// opsBucketsWithSamples reads one scope's per-minute buckets AND the latency
// samples behind them.
//
// Percentiles are not additive, so a merged view must collect the raw samples of
// every channel before computing one percentile. The earlier code asked the
// aggregator to summarise the scope label "all", whose own sample lists do not
// exist, which is why both P95 figures came back as zero whenever "all channels"
// was selected.
func (a *API) opsBucketsWithSamples(ctx context.Context, scope string, since, until time.Time) ([]opsagg.Bucket, []int64, []int64, error) {
	if scope != "all" {
		buckets, err := a.opsAggregator.Range(ctx, scope, since, until)
		if err != nil {
			return nil, nil, nil, err
		}
		durations, ttfts, err := a.opsAggregator.SamplesForChecked(ctx, scope, buckets)
		return buckets, durations, ttfts, err
	}

	channels, err := a.opsAggregator.Channels(ctx, since, until)
	if err != nil {
		return nil, nil, nil, err
	}
	merged := map[time.Time]opsagg.Bucket{}
	var durations, ttfts []int64
	for _, channel := range channels {
		// The infrastructure aggregates are not user traffic: the http catch-all
		// covers the admin UI, health checks and scanners, and probe is synthetic.
		if !IsProviderChannel(channel) {
			continue
		}
		buckets, err := a.opsAggregator.Range(ctx, channel, since, until)
		if err != nil {
			return nil, nil, nil, err
		}
		channelDurations, channelTTFTs, err := a.opsAggregator.SamplesForChecked(ctx, channel, buckets)
		if err != nil {
			return nil, nil, nil, err
		}
		durations = append(durations, channelDurations...)
		ttfts = append(ttfts, channelTTFTs...)

		for _, bucket := range buckets {
			combined := merged[bucket.Minute]
			combined.Minute = bucket.Minute
			combined.Counters.Add(bucket.Counters)
			combined.DurationFailed = append(combined.DurationFailed, bucket.DurationFailed...)
			combined.FirstTokenFailed = append(combined.FirstTokenFailed, bucket.FirstTokenFailed...)
			combined.DurationAttempt = append(combined.DurationAttempt, bucket.DurationAttempt...)
			combined.FirstTokenAttempt = append(combined.FirstTokenAttempt, bucket.FirstTokenAttempt...)
			combined.Requests += bucket.Requests
			combined.Success += bucket.Success
			combined.Failed += bucket.Failed
			combined.Input += bucket.Input
			combined.Cached += bucket.Cached
			combined.Reasoning += bucket.Reasoning
			if bucket.Total > 0 {
				combined.Total += bucket.Total
			} else {
				combined.Total += bucket.Input + bucket.Output
			}
			combined.PricedRequests += bucket.PricedRequests
			combined.UnpricedRequests += bucket.UnpricedRequests
			combined.PricedTokens += bucket.PricedTokens
			combined.UnpricedTokens += bucket.UnpricedTokens
			combined.CostInUSDTicks += bucket.CostInUSDTicks
			combined.Output += bucket.Output
			merged[bucket.Minute] = combined
		}
	}
	out := make([]opsagg.Bucket, 0, len(merged))
	for _, bucket := range merged {
		out = append(out, bucket)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Minute.Before(out[j].Minute) })
	return out, durations, ttfts, nil
}

func opsSeries(buckets []opsagg.Bucket) []map[string]interface{} {
	series := make([]map[string]interface{}, 0, len(buckets))
	for _, bucket := range buckets {
		series = append(series, map[string]interface{}{
			"minute":       bucket.Minute.UTC().Format(time.RFC3339),
			"requests":     bucket.Requests,
			"success":      bucket.Success,
			"failed":       bucket.Failed,
			"input_tokens": bucket.Input, "output_tokens": bucket.Output, "usage_samples": bucket.UsageSamples, "attempt_failures": bucket.AttemptFailures, "account_switch_count": bucket.AccountSwitchCount, "account_switch_sum": bucket.AccountSwitchSum,
		})
	}
	return series
}

// opsMatrix builds the channel × model status rows. A row with no samples is
// reported as such (samples = 0) instead of a green, traffic-free channel.
func (a *API) opsMatrix(ctx context.Context, channels []string, since, until time.Time) ([]map[string]interface{}, error) {
	accounts, err := a.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(accounts))
	for _, acc := range accounts {
		if acc != nil {
			ids = append(ids, acc.ID)
		}
	}
	counts := map[int64]int64{}
	capacityAvailable := false
	if a.connTracker != nil {
		if checked, ok := a.connTracker.(interface {
			GetCountsChecked(context.Context, []int64) (map[int64]int64, error)
		}); ok {
			counts, err = checked.GetCountsChecked(ctx, ids)
			if err != nil {
				return nil, err
			}
			capacityAvailable = true
		} else {
			counts = a.connTracker.GetCounts(ids)
		}
	}
	now := time.Now()
	// The rate is per minute of the requested window, not per bucket that exists.
	windowMinutes := until.Sub(since).Minutes()
	if windowMinutes <= 0 {
		windowMinutes = 1
	}

	rows := make([]map[string]interface{}, 0, len(channels))
	for _, channel := range channels {
		// The matrix answers "which provider channel × model is healthy", so the
		// infrastructure aggregates are not rows in it.
		if !IsProviderChannel(channel) {
			continue
		}
		pool := poolstate.Counts(accounts, channel, now, poolstate.NeedingLoginRefused)
		enabled, available, needingLogin, modelCooldowns := pool.Enabled, pool.Available, pool.NeedingLogin, pool.ModelCooldowns
		active := int64(0)
		for _, acc := range accounts {
			if acc != nil && strings.EqualFold(acc.AccountType, channel) {
				active += counts[acc.ID]
			}
		}
		row := map[string]interface{}{
			"channel":          channel,
			"accounts_enabled": enabled,
			"active_requests":  active, "concurrency_available": a.connTracker != nil,
			"accounts_available":     available,
			"accounts_needing_login": needingLogin,
			"model_cooldowns":        modelCooldowns,
			"models":                 []opsagg.ModelStats{},
		}
		row["capacity_available"] = false
		if capacityAvailable {
			maps.Copy(row, opsPoolCapacity(accounts, counts, channel))
		}
		if a.opsAggregator != nil && a.opsAggregator.Enabled() {
			buckets, err := a.opsAggregator.Range(ctx, channel, since, until)
			if err == nil {
				durations, ttfts, err := a.opsAggregator.SamplesForChecked(ctx, channel, buckets)
				if err != nil {
					return nil, err
				}
				summary := a.opsAggregator.SummarizeWith(ctx, opsagg.SummaryInput{
					Channel:         channel,
					Buckets:         buckets,
					WindowMinutes:   windowMinutes,
					Durations:       durations,
					FirstTokenMS:    ttfts,
					SamplesProvided: true,
				})
				row["summary"] = summary
				models, err := a.opsAggregator.ModelStatsFromBucketsChecked(ctx, channel, buckets)
				if err != nil {
					return nil, err
				}
				row["models"] = models
				row["has_sample"] = summary.Requests > 0
				row["series"] = opsSeries(buckets)
			} else {
				return nil, err
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (a *API) firingAlerts() []alerting.Alert {
	if a == nil || a.alertEngine == nil {
		return []alerting.Alert{}
	}
	alerts := a.alertEngine.Firing()
	if alerts == nil {
		return []alerting.Alert{}
	}
	return alerts
}

// currentConcurrency reports how many accounts are mid-refresh right now, if the
// deployment wired a reporter.
func (a *API) currentConcurrency() map[string]interface{} {
	value := 0
	if a != nil && a.refreshConcurrency != nil {
		value = a.refreshConcurrency()
	}
	return map[string]interface{}{"accounts_refreshing": value}
}

// parseOpsWindow resolves the requested window, clamped to what the buckets
// retain, and returns the resolved boundaries so the page can state them.
func (a *API) parseOpsWindow(r *http.Request) (int, time.Time, time.Time) {
	minutes := defaultOpsWindowMinutes
	if raw := strings.TrimSpace(r.URL.Query().Get("window")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			minutes = parsed
		}
	}
	if minutes > opsagg.MaxTrendMinutes {
		minutes = opsagg.MaxTrendMinutes
	}
	if maxMinutes := int(opsagg.BucketRetention.Minutes()); minutes > maxMinutes {
		minutes = maxMinutes
	}
	until := time.Now()
	if raw := strings.TrimSpace(r.URL.Query().Get("until")); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			until = parsed
		}
	}
	return minutes, until.Add(-time.Duration(minutes) * time.Minute), until
}

// opsChannels lists the provider channels the page may filter by, plus the
// infrastructure aggregates that were counted but left out, so the page can say
// what it excluded instead of silently dropping traffic from the picker.
func (a *API) opsChannels(ctx context.Context, since, until time.Time) ([]string, []string, error) {
	seen := map[string]bool{}
	if a.opsAggregator != nil && a.opsAggregator.Enabled() {
		observed, err := a.opsAggregator.Channels(ctx, since, until)
		if err != nil {
			return nil, nil, err
		}
		for _, channel := range observed {
			seen[channel] = true
		}
	}
	if a.store != nil {
		if accounts, err := a.store.ListAccounts(ctx); err == nil {
			for _, acc := range accounts {
				if acc == nil {
					continue
				}
				name := strings.ToLower(strings.TrimSpace(acc.AccountType))
				if name != "" {
					seen[name] = true
				}
			}
		}
	}
	channels := make([]string, 0, len(seen))
	aggregates := make([]string, 0, 2)
	for channel := range seen {
		if IsProviderChannel(channel) {
			channels = append(channels, channel)
			continue
		}
		aggregates = append(aggregates, channel)
	}
	sort.Strings(channels)
	sort.Strings(aggregates)
	return channels, aggregates, nil
}
