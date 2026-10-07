package httpclient

import (
	"context"
	"errors"
	"io"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
	"time"
)

type cancelAwareReadCloser struct {
	done <-chan struct{}
}

func (r *cancelAwareReadCloser) Read([]byte) (int, error) {
	<-r.done
	return 0, context.Canceled
}

func (r *cancelAwareReadCloser) Close() error { return nil }

func TestMonitorReadIdleCancelsBlockedReadWithClassifiableError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := MonitorReadIdle(&cancelAwareReadCloser{done: ctx.Done()}, 20*time.Millisecond, cancel, "grok")
	_, err := body.Read(make([]byte, 1))
	testutil.Falsef(t, err == nil || !strings.Contains(err.Error(), "grok stream idle timeout"), "Read() error=%v want Grok idle timeout", err)
	testutil.Falsef(t, !errors.Is(ctx.Err(), context.Canceled), "context error=%v want canceled", ctx.Err())
	_ = body.Close()
}

func TestMonitorReadIdleLeavesDisabledReaderUntouched(t *testing.T) {
	body := io.NopCloser(strings.NewReader("ok"))
	testutil.Equal(t, MonitorReadIdle(body, 0, func() {}, "workbuddy"), body)
}

func TestMonitorReadIdleDoesNotCountDownstreamBackpressure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := MonitorReadIdle(io.NopCloser(strings.NewReader("ok")), 20*time.Millisecond, cancel, "backpressure-test")
	defer body.Close()
	if _, err := body.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(70 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("consumer processing time canceled a healthy upstream")
	}
	if _, err := body.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
}

type heartbeatReader struct{ ctx context.Context }

func (r heartbeatReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-time.After(5 * time.Millisecond):
		return copy(p, ": heartbeat\n\n"), nil
	}
}
func (heartbeatReader) Close() error { return nil }

func TestProgressIdleRejectsHeartbeatOnlyStreams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, _ := MonitorProgressIdle(heartbeatReader{ctx}, 20*time.Millisecond, cancel, "heartbeat-test")
	defer body.Close()
	deadline := time.After(time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("heartbeat masked the stalled generation")
		default:
		}
		_, err := body.Read(make([]byte, 64))
		if err != nil {
			if !errors.Is(err, ErrStreamIdle("heartbeat-test")) {
				t.Fatal(err)
			}
			break
		}
	}
}
