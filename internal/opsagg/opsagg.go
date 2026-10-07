// Package opsagg aggregates request outcomes into per-minute Redis buckets so
// the overview can show trends without scanning the bounded audit stream.
//
// The audit stream is deliberately capped (a fixed maxlen), so "last 7 days"
// cannot be answered from it. These buckets are cheap, expire on their own, and
// carry their own retention window, which is what lets the UI state what the
// numbers actually cover.
package opsagg

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Bucket retention. 8 days at one bucket per minute is small (a few thousand
// keys at most) and covers a weekly comparison.
const (
	BucketRetention = 8 * 24 * time.Hour
	MaxTrendMinutes = 24 * 60
)

// Outcome is one finished request attributed to a channel, model and account.
type Outcome struct {
	OutputEvidence, VisibleOutput, ReasoningOnly, OutputTruncated bool
	HTTPStatus                                                    int
	Detailed, UsageReported, ProviderReached                      bool
	AttemptFailures, AccountSwitches                              int64
	Channel                                                       string
	Model                                                         string
	Status                                                        string
	// OK decides the success ratio. A retry that eventually succeeded is OK.
	OK bool
	// DurationMS is the whole request; FirstTokenMS is the first generated SSE
	// text, reasoning or tool event (non-streaming uses body TTFB). Visible text
	// latency is recorded separately in request journal metadata.
	DurationMS      int64
	FirstTokenMS    int64
	InputTokens     int64
	CachedTokens    int64
	OutputTokens    int64
	ReasoningTokens int64
	TotalTokens     int64
	// Priced marks a row whose usage came from the upstream and whose model has a
	// published price; unpriced rows are counted separately so a dashboard can
	// never present an estimate as a bill.
	Priced bool
	// CostInUSDTicks is the priced cost of this request in ticks (1 USD = 1e10).
	// It is zero for unpriced rows, which is a statement about the price table,
	// not about the request.
	CostInUSDTicks int64
	// At defaults to now.
	At time.Time
}

// Bucket is one minute of one channel.
type Bucket struct {
	Counters
	DurationFailed, FirstTokenFailed, DurationAttempt, FirstTokenAttempt []int64   `json:"-"`
	Channel                                                              string    `json:"channel"`
	Minute                                                               time.Time `json:"minute"`
	Requests                                                             int64     `json:"requests"`
	Success                                                              int64     `json:"success"`
	Failed                                                               int64     `json:"failed"`
	Input                                                                int64     `json:"input_tokens"`
	Cached                                                               int64     `json:"cached_input_tokens"`
	Output                                                               int64     `json:"output_tokens"`
	Reasoning                                                            int64     `json:"reasoning_tokens"`
	Total                                                                int64     `json:"total_tokens"`
	PricedRequests                                                       int64     `json:"priced_requests"`
	UnpricedRequests                                                     int64     `json:"unpriced_requests"`
	PricedTokens                                                         int64     `json:"priced_tokens"`
	UnpricedTokens                                                       int64     `json:"unpriced_tokens"`
	CostInUSDTicks                                                       int64     `json:"cost_in_usd_ticks"`
	DurationMS                                                           []int64   `json:"-"`
	ConcurrencyPeak                                                      int64     `json:"concurrency_peak"`
}

// Summary is a rolled-up view over a time range.
type Summary struct {
	Attributable   int64   `json:"attributable"`
	SLASuccessRate float64 `json:"sla_success_rate"`
	Counters
	QPS                      float64        `json:"qps"`
	TPS                      float64        `json:"tps"`
	Duration                 Distribution   `json:"duration"`
	FirstToken               Distribution   `json:"first_token"`
	DurationFailed           Distribution   `json:"duration_failed"`
	FirstTokenFailed         Distribution   `json:"first_token_failed"`
	DurationAttempt          Distribution   `json:"duration_attempt"`
	FirstTokenAttempt        Distribution   `json:"first_token_attempt"`
	DurationHistogram        []HistogramBin `json:"duration_histogram"`
	DurationHistogramFailed  []HistogramBin `json:"duration_histogram_failed"`
	DurationHistogramAttempt []HistogramBin `json:"duration_histogram_attempt"`

	Channel           string  `json:"channel"`
	Requests          int64   `json:"requests"`
	Success           int64   `json:"success"`
	Failed            int64   `json:"failed"`
	SuccessRate       float64 `json:"success_rate"`
	RPM               float64 `json:"rpm"`
	DurationP95MS     int64   `json:"duration_p95_ms"`
	FirstTokenP95MS   int64   `json:"first_token_p95_ms"`
	InputTokens       int64   `json:"input_tokens"`
	CachedInputTokens int64   `json:"cached_input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	ReasoningTokens   int64   `json:"reasoning_tokens"`
	TotalTokens       int64   `json:"total_tokens"`
	PricedRequests    int64   `json:"priced_requests"`
	UnpricedRequests  int64   `json:"unpriced_requests"`
	PricedTokens      int64   `json:"priced_tokens"`
	UnpricedTokens    int64   `json:"unpriced_tokens"`
	// CostInUSDTicks is the summed cost of the window, in ticks. A window that
	// mixes priced and unpriced rows reports both the cost and the unpriced
	// request count, so the figure is never presented as complete on its own.
	CostInUSDTicks int64 `json:"cost_in_usd_ticks"`
	// Samples reports how many observations backed the percentiles. Zero means
	// "no sample", which the UI must show as such rather than as healthy.
	Samples int64 `json:"samples"`
}

// Aggregator writes and reads the per-minute buckets.
type Aggregator struct {
	client      *redis.Client
	prefix      string
	written     atomic.Uint64
	writeFailed atomic.Uint64
}

// New creates an aggregator over the shared Redis client.
func New(client *redis.Client, prefix string) *Aggregator {
	if client == nil {
		return nil
	}
	if prefix == "" {
		prefix = "orchids:"
	}
	return &Aggregator{client: client, prefix: prefix}
}

// Enabled reports whether the aggregator has a backing store.
func (a *Aggregator) Enabled() bool { return a != nil && a.client != nil }

func (a *Aggregator) key(minute time.Time, channel string) string {
	return a.prefix + "ops:agg:" + strconv.FormatInt(minute.Unix()/60, 10) + ":" + normalizeChannel(channel)
}

func normalizeChannel(channel string) string {
	name := strings.ToLower(strings.TrimSpace(channel))
	if name == "" {
		return "unknown"
	}
	return name
}

// Observe pipelines one outcome with a bounded Redis timeout. A failure is
// best effort and must not change the inference response.
func (a *Aggregator) Observe(ctx context.Context, outcome Outcome) {
	if !a.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	at := outcome.At
	if at.IsZero() {
		at = time.Now()
	}
	minute := at.Truncate(time.Minute)
	key := a.key(minute, outcome.Channel)

	pipe := a.client.Pipeline()
	pipe.HIncrBy(ctx, key, "requests", 1)
	a.observeDetails(ctx, pipe, key, outcome)
	if outcome.OK {
		pipe.HIncrBy(ctx, key, "success", 1)
	} else {
		pipe.HIncrBy(ctx, key, "failed", 1)
	}
	if outcome.DurationMS > 0 {
		pipe.RPush(ctx, key+":dur", outcome.DurationMS)
	}
	if outcome.FirstTokenMS > 0 {
		pipe.RPush(ctx, key+":ttft", outcome.FirstTokenMS)
	}
	if outcome.InputTokens > 0 {
		pipe.HIncrBy(ctx, key, "input_tokens", outcome.InputTokens)
	}
	if outcome.OutputTokens > 0 {
		pipe.HIncrBy(ctx, key, "output_tokens", outcome.OutputTokens)
	}
	if outcome.CachedTokens > 0 {
		pipe.HIncrBy(ctx, key, "cached_input_tokens", outcome.CachedTokens)
	}
	if outcome.ReasoningTokens > 0 {
		pipe.HIncrBy(ctx, key, "reasoning_tokens", outcome.ReasoningTokens)
	}
	total := outcome.TotalTokens
	if total <= 0 {
		total = outcome.InputTokens + outcome.OutputTokens
	}
	if total > 0 {
		pipe.HIncrBy(ctx, key, "total_tokens", total)
	}
	if outcome.CostInUSDTicks > 0 {
		pipe.HIncrBy(ctx, key, "cost_in_usd_ticks", outcome.CostInUSDTicks)
	}
	if outcome.UsageReported {
		if outcome.Priced {
			pipe.HIncrBy(ctx, key, "priced_requests", 1)
			if total > 0 {
				pipe.HIncrBy(ctx, key, "priced_tokens", total)
			}
		} else {
			pipe.HIncrBy(ctx, key, "unpriced_requests", 1)
			if total > 0 {
				pipe.HIncrBy(ctx, key, "unpriced_tokens", total)
			}
		}
	}
	if outcome.Model != "" {
		// Per-model counters live in the same bucket: the matrix needs per-model
		// request/success figures and a second key per model would multiply the
		// key count for no benefit.
		modelField := "model:" + strings.TrimSpace(outcome.Model)
		pipe.HIncrBy(ctx, key, modelField+":requests", 1)
		if outcome.OK {
			pipe.HIncrBy(ctx, key, modelField+":success", 1)
		} else {
			pipe.HIncrBy(ctx, key, modelField+":failed", 1)
		}
		if outcome.FirstTokenMS > 0 {
			pipe.RPush(ctx, key+":"+modelField+":ttft", outcome.FirstTokenMS)
			pipe.Expire(ctx, key+":"+modelField+":ttft", BucketRetention)
			pipe.LTrim(ctx, key+":"+modelField+":ttft", -2000, -1)
		}
		if outcome.DurationMS > 0 {
			pipe.RPush(ctx, key+":"+modelField, outcome.DurationMS)
		}
	}
	pipe.Expire(ctx, key, BucketRetention)
	pipe.Expire(ctx, key+":dur", BucketRetention)
	pipe.Expire(ctx, key+":ttft", BucketRetention)
	if outcome.Model != "" {
		pipe.Expire(ctx, key+":model:"+strings.TrimSpace(outcome.Model), BucketRetention)
	}
	// Bounded list: percentiles over the most recent samples are enough and the
	// list must not grow with traffic.
	pipe.LTrim(ctx, key+":dur", -5000, -1)
	pipe.LTrim(ctx, key+":ttft", -5000, -1)
	if outcome.Model != "" {
		pipe.LTrim(ctx, key+":model:"+strings.TrimSpace(outcome.Model), -2000, -1)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		// Aggregation is best-effort observability; the request must not fail
		// because a counter could not be written. It is logged so a silently dead
		// overview is diagnosable instead of invisible.
		a.writeFailed.Add(1)
		slog.Warn("Ops aggregation write failed", "channel", normalizeChannel(outcome.Channel), "error", err)
		return
	}
	a.written.Add(1)
}

// Health reports writes in this process, separately from the retained counters.
func (a *Aggregator) Health() map[string]interface{} {
	if !a.Enabled() {
		return map[string]interface{}{"available": false}
	}
	return map[string]interface{}{"available": true, "written": a.written.Load(), "write_failed": a.writeFailed.Load()}
}

// Range reads the buckets of one channel between two instants (inclusive of the
// floor minute of From and To).
func (a *Aggregator) Range(ctx context.Context, channel string, from, to time.Time) ([]Bucket, error) {
	if !a.Enabled() {
		return nil, nil
	}
	if to.Before(from) {
		from, to = to, from
	}
	fromMinute, toMinute := from.Truncate(time.Minute), to.Truncate(time.Minute)
	minutes := int(toMinute.Sub(fromMinute)/time.Minute) + 1
	if minutes > MaxTrendMinutes {
		minutes = MaxTrendMinutes
		fromMinute = toMinute.Add(-time.Duration(minutes-1) * time.Minute)
	}

	// A daily window may contain 1440 buckets. Batch reads so the dashboard
	// does not spend one Redis round trip per minute; keep each batch bounded.
	const batchSize = 256
	buckets := make([]Bucket, 0, minutes)
	for start := 0; start < minutes; start += batchSize {
		count := min(batchSize, minutes-start)
		pipe := a.client.Pipeline()
		commands := make([]*redis.MapStringStringCmd, count)
		for i := range commands {
			minute := fromMinute.Add(time.Duration(start+i) * time.Minute)
			commands[i] = pipe.HGetAll(ctx, a.key(minute, channel))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, err
		}
		for i, command := range commands {
			if fields := command.Val(); len(fields) != 0 {
				minute := fromMinute.Add(time.Duration(start+i) * time.Minute)
				buckets = append(buckets, *bucketFromFields(minute, channel, fields))
			}
		}
	}
	return buckets, nil
}

// Channels lists the channels that have any bucket inside the range, so the UI
// offers real choices instead of a hardcoded list. A bucket key is
// "<prefix>ops:agg:<minute>:<channel>" and its side lists are "<...>:dur" and
// "<...>:ttft"; only keys that actually carry the request counter count as a
// channel, otherwise a percentile list would show up as a channel named "dur".
func (a *Aggregator) Channels(ctx context.Context, from, to time.Time) ([]string, error) {
	if !a.Enabled() {
		return nil, nil
	}
	if to.Before(from) {
		from, to = to, from
	}
	fromMinute := from.Truncate(time.Minute)
	toMinute := to.Truncate(time.Minute)
	seen := map[string]bool{}

	// Scan the namespace once instead of issuing KEYS for every minute in the
	// requested window. KEYS blocks Redis while it walks the entire keyspace;
	// SCAN is incremental and the minute encoded in each key lets us retain the
	// same inclusive range semantics as the old implementation.
	const scanCount int64 = 256
	var cursor uint64
	for {
		keys, next, err := a.client.Scan(ctx, cursor, a.prefix+"ops:agg:*", scanCount).Result()
		if err != nil {
			return nil, err
		}
		candidates := make([]struct{ key, channel string }, 0, len(keys))
		for _, key := range keys {
			minute, channel, ok := bucketKeyParts(a.prefix, key)
			if ok && !minute.Before(fromMinute) && !minute.After(toMinute) {
				candidates = append(candidates, struct{ key, channel string }{key, channel})
			}
		}
		for start := 0; start < len(candidates); start += 256 {
			end := start + 256
			if end > len(candidates) {
				end = len(candidates)
			}
			pipe := a.client.Pipeline()
			checks := make([]*redis.BoolCmd, 0, end-start)
			for _, candidate := range candidates[start:end] {
				checks = append(checks, pipe.HExists(ctx, candidate.key, "requests"))
			}
			if _, err := pipe.Exec(ctx); err != nil {
				return nil, err
			}
			for i, check := range checks {
				if check.Val() {
					seen[candidates[start+i].channel] = true
				}
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	channels := make([]string, 0, len(seen))
	for channel := range seen {
		channels = append(channels, channel)
	}
	sort.Strings(channels)
	return channels, nil
}

// bucketKeyParts parses a base bucket key and returns its minute and channel.
// Side lists and per-model keys are rejected by channelFromBucketKey.
func bucketKeyParts(prefix, key string) (time.Time, string, bool) {
	trimmed := strings.TrimPrefix(key, prefix+"ops:agg:")
	parts := strings.SplitN(trimmed, ":", 2)
	if len(parts) != 2 {
		return time.Time{}, "", false
	}
	epochMinute, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, "", false
	}
	channel, ok := channelFromBucketKey(prefix, key)
	if !ok {
		return time.Time{}, "", false
	}
	return time.Unix(epochMinute*60, 0), channel, true
}

// channelFromBucketKey extracts the channel from a bucket key, rejecting the
// percentile side lists and per-model lists.
func channelFromBucketKey(prefix, key string) (string, bool) {
	trimmed := strings.TrimPrefix(key, prefix+"ops:agg:")
	parts := strings.SplitN(trimmed, ":", 2)
	if len(parts) != 2 {
		return "", false
	}
	channel := parts[1]
	if channel == "" || isDetailSuffix(channel) {
		return "", false
	}
	if strings.HasSuffix(channel, ":dur") || strings.HasSuffix(channel, ":ttft") || strings.Contains(channel, ":model:") {
		return "", false
	}
	return channel, true
}

// SamplesForChecked batches bounded list reads and preserves read failures;
// an unavailable Redis is not a population with zero latency samples.
func (a *Aggregator) SamplesForChecked(ctx context.Context, channel string, buckets []Bucket) (durations []int64, ttfts []int64, err error) {
	if !a.Enabled() {
		return nil, nil, nil
	}
	suffixes := []string{":dur", ":ttft", ":dur_failed", ":ttft_failed", ":dur_attempt", ":ttft_attempt"}
	for start := 0; start < len(buckets); start += 32 {
		end := min(start+32, len(buckets))
		pipe := a.client.Pipeline()
		commands := make([][6]*redis.StringSliceCmd, end-start)
		for i := start; i < end; i++ {
			key := a.key(buckets[i].Minute, channel)
			for j, suffix := range suffixes {
				commands[i-start][j] = pipe.LRange(ctx, key+suffix, -5000, -1)
			}
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, nil, err
		}
		for i, reads := range commands {
			values := [6][]int64{}
			for j, read := range reads {
				for _, raw := range read.Val() {
					value, err := strconv.ParseInt(raw, 10, 64)
					if err != nil {
						return nil, nil, err
					}
					values[j] = append(values[j], value)
				}
			}
			durations = append(durations, values[0]...)
			ttfts = append(ttfts, values[1]...)
			bucket := &buckets[start+i]
			bucket.DurationFailed, bucket.FirstTokenFailed = values[2], values[3]
			bucket.DurationAttempt, bucket.FirstTokenAttempt = values[4], values[5]
		}
	}
	return durations, ttfts, nil
}

// bucketFromFields builds a bucket from the stored hash fields.
func bucketFromFields(minute time.Time, channel string, fields map[string]string) *Bucket {
	toInt := func(name string) int64 { value, _ := strconv.ParseInt(fields[name], 10, 64); return value }
	return &Bucket{
		Counters:         countersFrom(fields),
		Channel:          normalizeChannel(channel),
		Minute:           minute,
		Requests:         toInt("requests"),
		Success:          toInt("success"),
		Failed:           toInt("failed"),
		Input:            toInt("input_tokens"),
		Cached:           toInt("cached_input_tokens"),
		Output:           toInt("output_tokens"),
		Reasoning:        toInt("reasoning_tokens"),
		Total:            toInt("total_tokens"),
		PricedRequests:   toInt("priced_requests"),
		UnpricedRequests: toInt("unpriced_requests"),
		PricedTokens:     toInt("priced_tokens"),
		UnpricedTokens:   toInt("unpriced_tokens"),
		CostInUSDTicks:   toInt("cost_in_usd_ticks"),
		ConcurrencyPeak:  toInt("concurrency_peak"),
	}
}

// SummaryInput carries the two things a summary needs that the buckets alone do
// not contain: the actual length of the requested window, and (for a merged view)
// the latency samples gathered from every channel.
//
// Without the window the rate was "requests per bucket that happens to exist",
// which turned one request in a sixty-minute window into an RPM of 1 instead of
// 1/60.
type SummaryInput struct {
	// Channel is the scope label ("grok", or "all").
	Channel string
	// Buckets are the per-minute counters, chronological or not.
	Buckets []Bucket
	// WindowMinutes is the length of the requested window. Zero means "use the
	// span covered by the buckets".
	WindowMinutes float64
	// Durations and FirstTokenMS are the latency samples to use. When nil, the
	// samples are read from this channel's own lists; a merged view passes the
	// samples collected from every channel, because the per-minute list of the
	// scope label itself does not exist.
	Durations    []int64
	FirstTokenMS []int64
	// SamplesProvided marks Durations/FirstTokenMS as authoritative (possibly
	// empty) instead of "read the channel's own lists".
	SamplesProvided bool
}

// SummarizeWith folds buckets into one summary with an explicit window and samples.
func (a *Aggregator) SummarizeWith(ctx context.Context, input SummaryInput) Summary {
	summary := Summary{Channel: normalizeChannel(input.Channel)}
	var durations, ttfts, failedDur, failedTTFT, attemptDur, attemptTTFT []int64
	if input.SamplesProvided {
		durations = input.Durations
		ttfts = input.FirstTokenMS
	}
	for _, bucket := range input.Buckets {
		summary.Counters.Add(bucket.Counters)
		failedDur = append(failedDur, bucket.DurationFailed...)
		failedTTFT = append(failedTTFT, bucket.FirstTokenFailed...)
		attemptDur = append(attemptDur, bucket.DurationAttempt...)
		attemptTTFT = append(attemptTTFT, bucket.FirstTokenAttempt...)
		summary.Requests += bucket.Requests
		summary.Success += bucket.Success
		summary.Failed += bucket.Failed
		summary.InputTokens += bucket.Input
		summary.CachedInputTokens += bucket.Cached
		summary.OutputTokens += bucket.Output
		summary.ReasoningTokens += bucket.Reasoning
		if bucket.Total > 0 {
			summary.TotalTokens += bucket.Total
		} else {
			summary.TotalTokens += bucket.Input + bucket.Output
		}
		summary.PricedRequests += bucket.PricedRequests
		summary.UnpricedRequests += bucket.UnpricedRequests
		summary.PricedTokens += bucket.PricedTokens
		summary.CostInUSDTicks += bucket.CostInUSDTicks
		summary.UnpricedTokens += bucket.UnpricedTokens
		if !input.SamplesProvided && a.Enabled() {
			key := a.key(bucket.Minute, input.Channel)
			durations = append(durations, a.listInts(ctx, key+":dur")...)
			ttfts = append(ttfts, a.listInts(ctx, key+":ttft")...)
		}
	}

	real := summary.Requests
	if real > 0 {
		ratio := float64(summary.Success) / float64(real)
		if ratio > 1 {
			ratio = 1
		}
		summary.SuccessRate = ratio
	}

	// The rate is per minute of the WINDOW, not per bucket that happens to exist.
	windowMinutes := input.WindowMinutes
	if windowMinutes <= 0 {
		windowMinutes = float64(len(input.Buckets))
	}
	if windowMinutes > 0 {
		summary.RPM = float64(real) / windowMinutes
	}

	summary.Attributable = max(int64(0), real-summary.RateLimited-summary.Rejected-summary.QuotaExhausted)
	if summary.Attributable > 0 {
		summary.SLASuccessRate = min(1, float64(summary.Success)/float64(summary.Attributable))
	}
	summary.QPS = summary.RPM / 60
	if windowMinutes > 0 {
		summary.TPS = float64(summary.TotalTokens) / (windowMinutes * 60)
	}
	summary.Duration = distribution(durations)
	summary.FirstToken = distribution(ttfts)
	summary.DurationFailed = distribution(failedDur)
	summary.FirstTokenFailed = distribution(failedTTFT)
	summary.DurationAttempt = distribution(attemptDur)
	summary.FirstTokenAttempt = distribution(attemptTTFT)
	summary.DurationHistogram = histogram(durations)
	summary.DurationHistogramFailed = histogram(failedDur)
	summary.DurationHistogramAttempt = histogram(attemptDur)
	summary.Samples = int64(len(durations))
	summary.DurationP95MS = percentile(durations, 0.95)
	summary.FirstTokenP95MS = percentile(ttfts, 0.95)
	return summary
}

func (a *Aggregator) listInts(ctx context.Context, key string) []int64 {
	values, err := a.client.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil
	}
	out := make([]int64, 0, len(values))
	for _, value := range values {
		parsed, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr == nil {
			out = append(out, parsed)
		}
	}
	return out
}

// percentile returns the nearest-rank p-th percentile of the samples.
func percentile(values []int64, p float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(math.Ceil(float64(len(sorted))*p)) - 1
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	if rank < 0 {
		rank = 0
	}
	return sorted[rank]
}

// ModelStats is the per-model roll-up inside one channel.
type ModelStats struct {
	FirstTokenSamples int64   `json:"first_token_samples"`
	Model             string  `json:"model"`
	Requests          int64   `json:"requests"`
	Success           int64   `json:"success"`
	Failed            int64   `json:"failed"`
	SuccessRate       float64 `json:"success_rate"`
	FirstTokenP95MS   int64   `json:"first_token_p95_ms"`
	DurationP95MS     int64   `json:"duration_p95_ms"`
	Samples           int64   `json:"samples"`
}

// ModelStatsFromBucketsChecked extracts per-model counters and latency samples,
// preserving read failures so unavailable statistics cannot appear as empty.
func (a *Aggregator) ModelStatsFromBucketsChecked(ctx context.Context, channel string, buckets []Bucket) ([]ModelStats, error) {
	if !a.Enabled() || len(buckets) == 0 {
		return nil, nil
	}
	type acc struct {
		requests, success, failed int64
		duration, ttfts           []int64
	}
	byModel := map[string]*acc{}
	for _, bucket := range buckets {
		key := a.key(bucket.Minute, channel)
		fields, err := a.client.HGetAll(ctx, key).Result()
		if err != nil {
			return nil, err
		}
		bucketModels := map[string]*acc{}
		for field, value := range fields {
			if !strings.HasPrefix(field, "model:") {
				continue
			}
			trimmed := strings.TrimPrefix(field, "model:")
			index := strings.LastIndex(trimmed, ":")
			if index <= 0 {
				continue
			}
			model, metric := trimmed[:index], trimmed[index+1:]
			entry := bucketModels[model]
			if entry == nil {
				entry = byModel[model]
				if entry == nil {
					entry = &acc{}
					byModel[model] = entry
				}
				bucketModels[model] = entry
			}
			parsed, _ := strconv.ParseInt(value, 10, 64)
			switch metric {
			case "requests":
				entry.requests += parsed
			case "success":
				entry.success += parsed
			case "failed":
				entry.failed += parsed
			}
		}
		for model, entry := range bucketModels {
			duration, err := a.readListInts(ctx, key+":model:"+model)
			if err != nil {
				return nil, err
			}
			ttft, err := a.readListInts(ctx, key+":model:"+model+":ttft")
			if err != nil {
				return nil, err
			}
			entry.duration = append(entry.duration, duration...)
			entry.ttfts = append(entry.ttfts, ttft...)
		}
	}
	stats := make([]ModelStats, 0, len(byModel))
	for model, entry := range byModel {
		stat := ModelStats{
			Model:           model,
			Requests:        entry.requests,
			Success:         entry.success,
			Failed:          entry.failed,
			DurationP95MS:   percentile(entry.duration, 0.95),
			FirstTokenP95MS: percentile(entry.ttfts, 0.95), FirstTokenSamples: int64(len(entry.ttfts)),
			Samples: int64(len(entry.duration)),
		}
		if entry.requests > 0 {
			stat.SuccessRate = float64(entry.success) / float64(entry.requests)
		}
		stats = append(stats, stat)
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].Requests > stats[j].Requests })
	return stats, nil
}

func (a *Aggregator) readListInts(ctx context.Context, key string) ([]int64, error) {
	values, err := a.client.LRange(ctx, key, -5000, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(values))
	for _, raw := range values {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}
