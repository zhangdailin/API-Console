package handler

import (
	"sync"
	"testing"

	"orchids-api/internal/config"
)

func TestSetConfigIsSafeForConcurrentReaders(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 1}, nil)
	var readers sync.WaitGroup
	done := make(chan struct{})
	for range 8 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				got := h.configSnapshot().RequestTimeout
				if got != 1 && got != 2 {
					t.Errorf("RequestTimeout=%d", got)
					return
				}
			}
		})
	}
	for range 100 {
		h.SetConfig(&config.Config{RequestTimeout: 2})
		h.SetConfig(&config.Config{RequestTimeout: 1})
	}
	close(done)
	readers.Wait()
}
