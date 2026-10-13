package store

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"orchids-api/internal/testutil"
)

func TestModelDefaultFailurePreservesPrevious(t *testing.T) {
	s, mini := newTestRedisStore(t, "atomic-default:")
	old := &Model{Channel: "workbuddy", ModelID: "old", IsDefault: true}
	testutil.NoError(t, s.CreateModel(t.Context(), old))
	backend := s.models.(*redisStore)
	// A bad index would cause partial writes in an ordinary pipeline.
	mini.Del(backend.modelsChannelModelIDMapKey())
	mini.Set(backend.modelsChannelModelIDMapKey(), "wrong-type")
	testutil.Error(t, s.CreateModel(t.Context(), &Model{Channel: "workbuddy", ModelID: "new", IsDefault: true}))
	saved, err := s.GetModel(t.Context(), old.ID)
	testutil.NoError(t, err)
	testutil.True(t, saved.IsDefault)
	mini.Del(backend.modelsChannelModelIDMapKey())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	testutil.Error(t, s.UpdateModel(ctx, &Model{ID: old.ID, Channel: "workbuddy", ModelID: "changed", IsDefault: true}))
	saved, err = s.GetModel(t.Context(), old.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, saved.ModelID, "old")
}

func TestConcurrentModelDefaultsHaveOneWinnerPerChannel(t *testing.T) {
	s, _ := newTestRedisStore(t, "concurrent-default:")
	other := &Model{Channel: "grok", ModelID: "other", IsDefault: true}
	testutil.NoError(t, s.CreateModel(t.Context(), other))
	start := make(chan struct{})
	failures := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			failures <- s.CreateModel(t.Context(), &Model{Channel: "workbuddy", ModelID: fmt.Sprintf("m-%d", i), IsDefault: true})
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		testutil.NoError(t, err)
	}
	rows, err := s.ListModels(t.Context())
	testutil.NoError(t, err)
	counts := map[string]int{}
	for _, row := range rows {
		if row.IsDefault {
			counts[row.Channel]++
		}
	}
	testutil.Equal(t, counts["workbuddy"], 1)
	testutil.Equal(t, counts["grok"], 1)
}
