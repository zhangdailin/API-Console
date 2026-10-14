package audit

import (
	"context"
	"orchids-api/internal/testutil"
	"strings"
	"sync"
	"testing"
)

func TestSinkHealthReportsPersistenceFailure(t *testing.T) {
	logger, server := setupRedisLogger(t)
	server.Set(logger.streamKey, "wrong type")
	logger.Log(context.Background(), Event{Action: "test"})
	logger.Close()
	health := logger.Health()
	testutil.Falsef(t, health.WriteFailed != 1 || health.Written != 0 || health.LastFailure == nil || health.Queue != 0 || health.QueuedBytes != 0, "health = %+v", health)
}

func TestSinkRejectsOversizeAndSnapshotsMetadata(t *testing.T) {
	logger, _ := setupRedisLogger(t)
	logger.Log(context.Background(), Event{Details: strings.Repeat("x", maxEventBytes+1)})
	metadata := map[string]interface{}{"value": "original"}
	logger.Log(context.Background(), Event{Action: "test", Metadata: metadata})
	metadata["value"] = "changed"
	logger.Close()
	logger.Close()
	logger.Log(context.Background(), Event{Action: "after-close"})
	health := logger.Health()
	testutil.Falsef(t, health.Dropped != 2 || health.Written != 1 || health.LastSuccess == nil, "health = %+v", health)
	events := readLoggedEvents(t, logger, 1)
	testutil.Equal(t, events[0].Metadata["value"], "original")
}

func TestSinkConcurrentCloseAndLog(t *testing.T) {
	logger, _ := setupRedisLogger(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				logger.Log(context.Background(), Event{Action: "concurrent"})
			}
		})
	}
	logger.Close()
	wg.Wait()
	health := logger.Health()
	testutil.Equal(t, health.Written+health.Dropped, 800)
}
