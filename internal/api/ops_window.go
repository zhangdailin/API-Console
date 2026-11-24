package api

import (
	"context"
	"sort"
	"time"

	"orchids-api/internal/opsagg"
)

type opsChannelData struct {
	buckets          []opsagg.Bucket
	durations, ttfts []int64
}

// opsWindow keeps one consistent read of each channel for the overview and its matrix.
type opsWindow struct {
	agg          *opsagg.Aggregator
	since, until time.Time
	channels     []string
	loaded       map[string]opsChannelData
}

func (w *opsWindow) read(ctx context.Context, channel string) (opsChannelData, error) {
	if data, ok := w.loaded[channel]; ok {
		return data, nil
	}
	var data opsChannelData
	buckets, err := w.agg.Range(ctx, channel, w.since, w.until)
	if err != nil {
		return data, err
	}
	durations, ttfts, err := w.agg.SamplesForChecked(ctx, channel, buckets)
	if err != nil {
		return data, err
	}
	data = opsChannelData{buckets: buckets, durations: durations, ttfts: ttfts}
	if w.loaded == nil {
		w.loaded = make(map[string]opsChannelData)
	}
	w.loaded[channel] = data
	return data, nil
}

// Percentiles are not additive: merge raw samples across the selected channels.
func (w *opsWindow) bucketsWithSamples(ctx context.Context, scope string) ([]opsagg.Bucket, []int64, []int64, error) {
	if scope != "all" {
		data, err := w.read(ctx, scope)
		return data.buckets, data.durations, data.ttfts, err
	}
	channels := w.channels
	merged := map[time.Time]opsagg.Bucket{}
	var durations, ttfts []int64
	for _, channel := range channels {
		// The infrastructure aggregates are not user traffic: the http catch-all
		// covers the admin UI, health checks and scanners, and probe is synthetic.
		if !IsProviderChannel(channel) {
			continue
		}
		data, err := w.read(ctx, channel)
		if err != nil {
			return nil, nil, nil, err
		}
		durations = append(durations, data.durations...)
		ttfts = append(ttfts, data.ttfts...)

		for _, bucket := range data.buckets {
			combined := merged[bucket.Minute]
			combined.Minute = bucket.Minute
			combined.Merge(bucket)
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
