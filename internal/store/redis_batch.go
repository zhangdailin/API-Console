package store

import (
	"errors"
	"fmt"
	"runtime"
	"slices"
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

func redisBatchKeys(ids []int64, key func(int64) string) []string {
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = key(id)
	}
	return keys
}

// decodeRedisRows keeps Redis row order and skips missing or filtered records.
func decodeRedisRows[T any](values []any, ids []int64, kind string, decode func(int, string) (*T, error)) ([]*T, error) {
	rows := make([]*T, len(values))
	if err := forEachIndex(len(values), func(i int) error {
		if values[i] == nil {
			return nil
		}
		raw, ok := values[i].(string)
		if !ok || raw == "" {
			return fmt.Errorf("invalid %s record #%d", kind, ids[i])
		}
		var err error
		rows[i], err = decode(i, raw)
		return err
	}); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(rows, func(row *T) bool { return row == nil }), nil
}

func parseSortedInt64s(values []string) []int64 {
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		if id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}
