package debug

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"orchids-api/internal/util"
	"strings"
	"time"
)

// UpstreamAttempt owns immutable section names, so concurrent or retried calls
// cannot attribute a previous response to the latest request.
type UpstreamAttempt struct {
	capture *Capture
	prefix  string
	started time.Time
	latency *Latency
}

func BeginUpstream(ctx context.Context, method, url string, headers http.Header, body interface{}) *UpstreamAttempt {
	return beginUpstream(FromContext(ctx), method, url, headers, body)
}
func beginUpstream(c *Capture, method, url string, headers http.Header, body interface{}) *UpstreamAttempt {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.attempts++
	id := c.attempts
	c.mu.Unlock()
	a := &UpstreamAttempt{capture: c, prefix: fmt.Sprintf("upstream_%03d_", id), started: time.Now()}
	if raw, ok := body.([]byte); ok {
		if json.Valid(raw) {
			body = json.RawMessage(raw)
		} else {
			body = string(raw)
		}
	}
	safeHeaders := map[string]string{}
	for k, v := range headers {
		switch strings.ToLower(k) {
		case "authorization", "proxy-authorization", "cookie", "cosy-key", "cosy-user", "cosy-machineid", "cosy-machinetoken":
			safeHeaders[k] = "[REDACTED]"
		default:
			safeHeaders[k] = strings.Join(v, ", ")
		}
	}
	a.writeJSON("request.json", map[string]interface{}{"attempt": id, "method": method, "url": url, "headers": safeHeaders, "body": body})
	return a
}
func (a *UpstreamAttempt) TraceRequest(req *http.Request) *http.Request {
	req = util.TraceHTTPPhases(req)
	if a == nil {
		return req
	}
	ctx, l := a.Trace(req.Context(), map[string]interface{}{"kind": "http", "host": req.URL.Host})
	a.latency = l
	return req.WithContext(ctx)
}
func (a *UpstreamAttempt) writeJSON(suffix string, value interface{}) {
	if a == nil {
		return
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err == nil {
		a.capture.Set(a.prefix+suffix, string(raw))
	}
}
func (a *UpstreamAttempt) Response(resp *http.Response, err error) {
	if a == nil {
		return
	}
	if a.latency != nil {
		if resp != nil {
			a.latency.Response(resp)
		}
		if err != nil {
			a.latency.Finish(err)
		}
	}
	result := map[string]interface{}{"elapsed_ms": time.Since(a.started).Milliseconds()}
	if resp != nil {
		result["status"] = resp.StatusCode
		result["content_type"] = resp.Header.Get("Content-Type")
	}
	if err != nil {
		result["error"] = err.Error()
	}
	a.writeJSON("result.json", result)
}
func (a *UpstreamAttempt) Append(data string) {
	if a != nil {
		a.capture.Append(a.prefix+"response.txt", data)
	}
}
func (a *UpstreamAttempt) CaptureBody(body io.ReadCloser) io.ReadCloser {
	if a == nil || body == nil {
		return body
	}
	return &attemptBody{ReadCloser: body, attempt: a}
}

type attemptBody struct {
	io.ReadCloser
	attempt  *UpstreamAttempt
	bytes    int
	complete bool
}

func (b *attemptBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.attempt.latency.Mark("first_body_ms")
	}
	b.bytes += n
	b.attempt.Append(string(p[:n]))
	if err == io.EOF {
		b.complete = true
		b.attempt.latency.Finish(nil)
	}
	if err != nil && err != io.EOF {
		b.attempt.writeJSON("read_error.json", map[string]interface{}{"error": err.Error()})
	}
	return n, err
}

func (b *attemptBody) Close() error {
	err := b.ReadCloser.Close()
	b.attempt.latency.Finish(err)
	b.attempt.writeJSON("body_state.json", map[string]interface{}{"bytes": b.bytes, "complete": b.complete, "note": "complete 表示读到上游 EOF；提前结束或取消不代表诊断按长度截断"})
	return err
}
