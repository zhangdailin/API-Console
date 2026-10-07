//go:build !windows

package handler

import "time"

var relayBenchEpoch = time.Now()

func relayBenchNow() int64                { return time.Since(relayBenchEpoch).Nanoseconds() }
func relayBenchElapsed(start int64) int64 { return relayBenchNow() - start }
