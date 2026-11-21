package api

import (
	"context"
	"orchids-api/internal/opsagg"
	"orchids-api/internal/testutil"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestMergedMetricsKeepUsageAndPricing(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	agg := opsagg.New(client, "merge:")
	now := time.Now()
	for _, channel := range []string{"grok", "cline"} {
		agg.Observe(context.Background(), opsagg.Outcome{Channel: channel, OK: true, At: now, UsageReported: true, Priced: true, InputTokens: 10, CachedTokens: 4, OutputTokens: 5, ReasoningTokens: 3, TotalTokens: 15, CostInUSDTicks: 100})
	}
	window := &opsWindow{agg: agg, since: now.Add(-time.Minute), until: now, channels: []string{"grok", "cline"}}
	buckets, dur, ttft, err := window.bucketsWithSamples(context.Background(), "all")
	testutil.NoError(t, err)
	reads := mini.CommandCount()
	individual, err := window.read(context.Background(), "grok")
	testutil.NoError(t, err)
	if len(individual.buckets) != 1 || mini.CommandCount() != reads {
		t.Fatalf("matrix channel read repeated Redis work: %d additional commands", mini.CommandCount()-reads)
	}
	summary := agg.SummarizeWith(context.Background(), opsagg.SummaryInput{Channel: "all", Buckets: buckets, Durations: dur, FirstTokenMS: ttft, SamplesProvided: true, WindowMinutes: 1})
	testutil.Falsef(t, summary.Requests != 2 || summary.CachedInputTokens != 8 || summary.ReasoningTokens != 6 || summary.TotalTokens != 30 || summary.CostInUSDTicks != 200 || summary.PricedRequests != 2 || summary.PricedTokens != 30, "merged usage lost: %+v", summary)
}
