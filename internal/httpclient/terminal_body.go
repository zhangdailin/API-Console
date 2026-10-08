package httpclient

import (
	"context"
	"io"
	"net/http"
)

// DrainTerminalResponse reads an already validated terminal HTTP/1 stream to
// transport EOF before Close and request cancellation. It adds no byte or time
// budget; the response's original request context still governs cancellation.
// Trailing bytes are transport cleanup, not additional protocol events. Failure
// to drain sacrifices reuse without changing the completed generation's result.
func DrainTerminalResponse(ctx context.Context, resp *http.Response) {
	if ctx == nil || ctx.Err() != nil || resp == nil || resp.Body == nil || resp.ProtoMajor != 1 || resp.Close {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}
