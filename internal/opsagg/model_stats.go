package opsagg

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

type modelBucketCounts struct{ requests, success, failed int64 }

func decodeModelBucketCounts(fields map[string]string) map[string]modelBucketCounts {
	var models map[string]modelBucketCounts
	for field, value := range fields {
		key, valid := strings.CutPrefix(field, "model:")
		i := strings.LastIndexByte(key, ':')
		if !valid || i <= 0 {
			continue
		}
		model, metric := key[:i], key[i+1:]
		counts := models[model]
		parsed, _ := strconv.ParseInt(value, 10, 64)
		switch metric {
		case "requests":
			counts.requests = parsed
		case "success":
			counts.success = parsed
		case "failed":
			counts.failed = parsed
		}
		if models == nil {
			models = make(map[string]modelBucketCounts)
		}
		models[model] = counts
	}
	return models
}

// ModelStatsFromBucketsChecked reuses counters captured by Range and batches
// bounded sample reads. Redis errors are never reported as zero observations.
func (a *Aggregator) ModelStatsFromBucketsChecked(ctx context.Context, channel string, buckets []Bucket) ([]ModelStats, error) {
	if !a.Enabled() || len(buckets) == 0 {
		return nil, nil
	}
	type acc struct {
		modelBucketCounts
		duration, ttfts []int64
	}
	type request struct {
		key   string
		entry *acc
	}
	const sampleBatch = 64
	requests := make([]request, 0, sampleBatch)
	byModel := map[string]*acc{}
	flush := func() error {
		if len(requests) == 0 {
			return nil
		}
		pipe := a.client.Pipeline()
		commands := make([][2]*redis.StringSliceCmd, len(requests))
		for i, request := range requests {
			commands[i] = [2]*redis.StringSliceCmd{pipe.LRange(ctx, request.key, -5000, -1), pipe.LRange(ctx, request.key+":ttft", -5000, -1)}
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return err
		}
		for i, reads := range commands {
			entry := requests[i].entry
			for j, target := range []*[]int64{&entry.duration, &entry.ttfts} {
				for _, value := range reads[j].Val() {
					n, err := strconv.ParseInt(value, 10, 64)
					if err != nil {
						return err
					}
					*target = append(*target, n)
				}
			}
		}
		requests = requests[:0]
		return nil
	}
	for _, bucket := range buckets {
		key := a.key(bucket.Minute, channel)
		for name, counts := range bucket.modelCounts {
			entry := byModel[name]
			if entry == nil {
				entry = &acc{}
				byModel[name] = entry
			}
			entry.requests += counts.requests
			entry.success += counts.success
			entry.failed += counts.failed
			requests = append(requests, request{key: key + ":model:" + name, entry: entry})
			if len(requests) == sampleBatch {
				if err := flush(); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
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
