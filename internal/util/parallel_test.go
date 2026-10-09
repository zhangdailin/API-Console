package util

import (
	"context"
	"orchids-api/internal/testutil"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunIndexed(t *testing.T) {
	for _, workers := range []int{1, 4} {
		seen := make([]int, 100)
		var active, peak atomic.Int32
		errs := RunIndexed(len(seen), workers, func(index int) error {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				old := peak.Load()
				if current <= old || peak.CompareAndSwap(old, current) {
					break
				}
			}
			seen[index]++
			if index == 3 {
				panic("secret value")
			}
			if index == 7 {
				return context.Canceled
			}
			time.Sleep(time.Millisecond)
			return nil
		})
		for index, count := range seen {
			if count != 1 {
				t.Fatalf("index %d ran %d times", index, count)
			}
		}
		if peak.Load() > int32(workers) {
			t.Fatal("concurrency limit exceeded")
		}
		for index, err := range errs {
			switch index {
			case 3:
				if err != ErrTaskPanic {
					t.Fatal(err)
				}
			case 7:
				if err != context.Canceled {
					t.Fatal(err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if RunIndexed(0, 4, func(int) error { t.Fatal("empty task executed"); return nil }) != nil {
		t.Fatal("empty result")
	}
}

func TestSleepWithContext(t *testing.T) {
	t.Run("zero duration", func(t *testing.T) {
		testutil.CheckFalse(t, !SleepWithContext(context.Background(), 0), "SleepWithContext(0) = false, want true")
	})

	t.Run("negative duration", func(t *testing.T) {
		testutil.CheckFalse(t, !SleepWithContext(context.Background(), -time.Second), "SleepWithContext(-1s) = false, want true")
	})

	t.Run("normal sleep", func(t *testing.T) {
		start := time.Now()
		testutil.CheckFalse(t, !SleepWithContext(context.Background(), 50*time.Millisecond), "SleepWithContext() = false, want true")
		elapsed := time.Since(start)
		testutil.CheckFalsef(t, elapsed < 40*time.Millisecond, "elapsed = %v, want >= 40ms", elapsed)
	})

	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		testutil.CheckFalse(t, SleepWithContext(ctx, 1*time.Second), "SleepWithContext() = true, want false for canceled context")
		elapsed := time.Since(start)
		testutil.CheckFalsef(t, elapsed > 100*time.Millisecond, "elapsed = %v, want < 100ms", elapsed)
	})
}
