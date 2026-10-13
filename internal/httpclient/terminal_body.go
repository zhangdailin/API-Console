package httpclient

import (
	"context"
	"io"
	"net/http"
	"time"
)

// DrainTerminalResponse reads an already validated terminal HTTP/1 stream to
// transport EOF before Close and request cancellation, within a short cleanup
// budget. Slow or excessive tails sacrifice reuse instead of delaying success.
// Trailing bytes are transport cleanup, not additional protocol events. Failure
// to drain sacrifices reuse without changing the completed generation's result.
func DrainTerminalResponse(ctx context.Context, resp *http.Response) {
	if ctx == nil || ctx.Err() != nil || resp == nil || resp.Body == nil || resp.ProtoMajor != 1 || resp.Close {
		return
	}
	drainTerminalResponse(ctx, resp, time.Second, 256<<10)
}

func drainTerminalResponse(ctx context.Context, resp *http.Response, timeout time.Duration, limit int64) {
	done := make(chan error, 1)
	go func() {
		_, err := io.CopyN(io.Discard, resp.Body, limit+1)
		done <- err
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err == io.EOF {
			return // Complete transport EOF allows ordinary HTTP/1 reuse.
		}
	case <-ctx.Done():
	case <-timer.C:
	}
	// net/http response bodies unblock an in-flight Read when closed.
	_ = resp.Body.Close()
}
