package store

import (
	"errors"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"orchids-api/internal/util"
)

// redisBatchParallelThreshold is the batch size above which fanning a decode
// loop out over the shared worker pool costs less than the scheduling it adds.
const redisBatchParallelThreshold = 32

// forEachIndex visits every position once, fanning the work out over the shared
// worker pool only when the batch is large enough to be worth the scheduling.
func forEachIndex(count int, visit func(int) error) error {
	workers := 1
	if count >= redisBatchParallelThreshold {
		workers = runtime.GOMAXPROCS(0)
	}
	return errors.Join(util.RunIndexed(count, workers, visit)...)
}

// compactNonNil keeps the order of the decoded rows while dropping the positions
// a missing or skipped row left empty.
func compactNonNil[T any](items []*T) []*T {
	out := make([]*T, 0, len(items))
	for _, item := range items {
		if item != nil {
			out = append(out, item)
		}
	}
	return out
}

func parseSortedInt64s(values []string) []int64 {
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		if id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
