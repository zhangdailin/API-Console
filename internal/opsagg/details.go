package opsagg

import (
	"context"
	"slices"
	"strconv"

	"github.com/redis/go-redis/v9"
)

type Counters struct {
	OutputEvidenceSamples int64 `json:"output_evidence_samples"`
	VisibleAnswers        int64 `json:"visible_answers"`
	ReasoningOnly         int64 `json:"reasoning_only"`
	OutputTruncated       int64 `json:"output_truncated"`
	DetailedRequests      int64 `json:"detailed_requests"`
	UsageSamples          int64 `json:"usage_samples"`
	AttemptFailures       int64 `json:"attempt_failures"`
	AccountSwitchSum      int64 `json:"account_switch_sum"`
	AccountSwitchCount    int64 `json:"account_switch_count"`
	ClientErrors          int64 `json:"client_errors"`
	ServerErrors          int64 `json:"server_errors"`
	StreamErrors          int64 `json:"stream_errors"`
	UpstreamAuth          int64 `json:"upstream_auth"`
	RateLimited           int64 `json:"rate_limited"`
	Rejected              int64 `json:"rejected"`
	QuotaExhausted        int64 `json:"quota_exhausted"`
}

func (c *Counters) Add(other Counters) {
	c.OutputEvidenceSamples += other.OutputEvidenceSamples
	c.VisibleAnswers += other.VisibleAnswers
	c.ReasoningOnly += other.ReasoningOnly
	c.OutputTruncated += other.OutputTruncated
	c.DetailedRequests += other.DetailedRequests
	c.UsageSamples += other.UsageSamples
	c.AttemptFailures += other.AttemptFailures
	c.AccountSwitchSum += other.AccountSwitchSum
	c.AccountSwitchCount += other.AccountSwitchCount
	c.ClientErrors += other.ClientErrors
	c.ServerErrors += other.ServerErrors
	c.StreamErrors += other.StreamErrors
	c.UpstreamAuth += other.UpstreamAuth
	c.RateLimited += other.RateLimited
	c.Rejected += other.Rejected
	c.QuotaExhausted += other.QuotaExhausted
}
func (c Counters) Values() map[string]int64 {
	return map[string]int64{"output_evidence_samples": c.OutputEvidenceSamples, "visible_answers": c.VisibleAnswers, "reasoning_only": c.ReasoningOnly, "output_truncated": c.OutputTruncated, "detailed_requests": c.DetailedRequests, "usage_samples": c.UsageSamples, "attempt_failures": c.AttemptFailures, "account_switch_sum": c.AccountSwitchSum, "account_switch_count": c.AccountSwitchCount, "client_errors": c.ClientErrors, "server_errors": c.ServerErrors, "stream_errors": c.StreamErrors, "upstream_auth": c.UpstreamAuth, "rate_limited": c.RateLimited, "rejected": c.Rejected, "quota_exhausted": c.QuotaExhausted}
}
func countersFrom(fields map[string]string) Counters {
	var c Counters
	c.OutputEvidenceSamples, _ = strconv.ParseInt(fields["output_evidence_samples"], 10, 64)
	c.VisibleAnswers, _ = strconv.ParseInt(fields["visible_answers"], 10, 64)
	c.ReasoningOnly, _ = strconv.ParseInt(fields["reasoning_only"], 10, 64)
	c.OutputTruncated, _ = strconv.ParseInt(fields["output_truncated"], 10, 64)
	c.DetailedRequests, _ = strconv.ParseInt(fields["detailed_requests"], 10, 64)
	c.UsageSamples, _ = strconv.ParseInt(fields["usage_samples"], 10, 64)
	c.AttemptFailures, _ = strconv.ParseInt(fields["attempt_failures"], 10, 64)
	c.AccountSwitchSum, _ = strconv.ParseInt(fields["account_switch_sum"], 10, 64)
	c.AccountSwitchCount, _ = strconv.ParseInt(fields["account_switch_count"], 10, 64)
	c.ClientErrors, _ = strconv.ParseInt(fields["client_errors"], 10, 64)
	c.ServerErrors, _ = strconv.ParseInt(fields["server_errors"], 10, 64)
	c.StreamErrors, _ = strconv.ParseInt(fields["stream_errors"], 10, 64)
	c.UpstreamAuth, _ = strconv.ParseInt(fields["upstream_auth"], 10, 64)
	c.RateLimited, _ = strconv.ParseInt(fields["rate_limited"], 10, 64)
	c.Rejected, _ = strconv.ParseInt(fields["rejected"], 10, 64)
	c.QuotaExhausted, _ = strconv.ParseInt(fields["quota_exhausted"], 10, 64)
	return c
}

type Distribution struct {
	Samples int     `json:"samples"`
	P50     int64   `json:"p50_ms"`
	P90     int64   `json:"p90_ms"`
	P95     int64   `json:"p95_ms"`
	P99     int64   `json:"p99_ms"`
	Avg     float64 `json:"avg_ms"`
	Max     int64   `json:"max_ms"`
}
type HistogramBin struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

func distribution(values []int64) Distribution {
	d := Distribution{Samples: len(values)}
	if len(values) == 0 {
		return d
	}
	for _, v := range values {
		d.Avg += float64(v)
		if v > d.Max {
			d.Max = v
		}
	}
	d.Avg /= float64(len(values))
	d.P50 = percentile(values, .5)
	d.P90 = percentile(values, .9)
	d.P95 = percentile(values, .95)
	d.P99 = percentile(values, .99)
	return d
}
func histogram(values []int64) []HistogramBin {
	out := []HistogramBin{{Label: "<1s"}, {Label: "1–5s"}, {Label: "5–15s"}, {Label: "15–60s"}, {Label: "≥60s"}}
	for _, v := range values {
		switch {
		case v < 1000:
			out[0].Count++
		case v < 5000:
			out[1].Count++
		case v < 15000:
			out[2].Count++
		case v < 60000:
			out[3].Count++
		default:
			out[4].Count++
		}
	}
	return out
}
func (a *Aggregator) observeDetails(ctx context.Context, pipe redis.Pipeliner, key string, o Outcome) {
	if !o.Detailed {
		return
	}
	c := Counters{DetailedRequests: 1, AttemptFailures: o.AttemptFailures, AccountSwitchSum: o.AccountSwitches, AccountSwitchCount: 1}
	if o.OutputEvidence {
		c.OutputEvidenceSamples = 1
	}
	if o.OK && o.VisibleOutput {
		c.VisibleAnswers = 1
	}
	if o.OK && o.ReasoningOnly {
		c.ReasoningOnly = 1
	}
	if o.OutputTruncated {
		c.OutputTruncated = 1
	}
	if o.UsageReported {
		c.UsageSamples = 1
	}
	if !o.OK {
		switch {
		case o.Status == "stream_error":
			c.StreamErrors = 1
		case o.HTTPStatus == 429 || o.HTTPStatus == 529:
			c.RateLimited = 1
		case o.HTTPStatus == 402:
			c.QuotaExhausted = 1
		case o.HTTPStatus == 401 || o.HTTPStatus == 403:
			if o.ProviderReached {
				c.UpstreamAuth = 1
			} else {
				c.Rejected = 1
			}
		case o.HTTPStatus >= 500:
			c.ServerErrors = 1
		case o.HTTPStatus >= 400:
			c.ClientErrors = 1
		}
	}
	for field, value := range c.Values() {
		if value != 0 {
			pipe.HIncrBy(ctx, key, field, value)
		}
	}
	for _, cohort := range []struct {
		name   string
		active bool
	}{{"failed", !o.OK}, {"attempt", o.AttemptFailures > 0}} {
		if !cohort.active {
			continue
		}
		for _, metric := range []struct {
			name  string
			value int64
		}{{"dur", o.DurationMS}, {"ttft", o.FirstTokenMS}} {
			if metric.value <= 0 {
				continue
			}
			k := key + ":" + metric.name + "_" + cohort.name
			pipe.RPush(ctx, k, metric.value)
			pipe.LTrim(ctx, k, -5000, -1)
			pipe.Expire(ctx, k, BucketRetention)
		}
	}
}

// Keep discovery on the request hashes; side lists are not channels.
func isDetailSuffix(channel string) bool {
	return slices.ContainsFunc([]string{":dur_failed", ":ttft_failed", ":dur_attempt", ":ttft_attempt"}, func(suffix string) bool {
		return len(channel) >= len(suffix) && channel[len(channel)-len(suffix):] == suffix
	})
}
