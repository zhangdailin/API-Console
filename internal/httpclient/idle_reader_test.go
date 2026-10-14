package httpclient

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestProgressIdleLeavesDisabledReaderUntouched(t *testing.T) {
	body := io.NopCloser(strings.NewReader("ok"))
	monitored, progress := MonitorProgressIdle(body, 0, func() {}, "disabled")
	if monitored != body {
		t.Fatal("disabled monitor wrapped the body")
	}
	progress()
}

func TestProgressIdleDoesNotCountDownstreamBackpressure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, progress := MonitorProgressIdle(io.NopCloser(strings.NewReader("ok")), 20*time.Millisecond, cancel, "backpressure")
	defer body.Close()
	if _, err := body.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	progress()
	time.Sleep(70 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("consumer processing canceled a healthy upstream")
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
