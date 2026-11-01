package qoder

// The model admission queue, as the desktop client reads it.
//
// A 10605 refusal carrying queueType p3|p4 is not a blind wait: the client asks
// the same signed surface what the queue looks like — requestSetId, modelKey and
// the queue class — and the answer names the wait (retryAfterSeconds, waitTime),
// the queue depth and whether the service is available at all. The gateway used
// to wait out a window it had inferred from measurements; this reads the number
// the server publishes.
//
// The read is the same COSY-signed control-plane GET as the catalog: the
// signature covers the signed path, so it needs no chat body.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/httpclient"
)

// The client names this path without the /algo prefix because its inference
// endpoint base already carries it, the same way the chat path is published as
// /algo/api/v2/service/pro/sse/agent_chat_generation.
const queueStatusPath = "/algo/api/v2/service/ask/queue/status"

// QueueStatus is the server's view of one model's admission queue.
type QueueStatus struct {
	Observed         bool
	IsQueued         bool
	ModelKey         string
	QueueType        string
	QueueCount       int
	ServiceAvailable bool
	RetryAfter       time.Duration
	WaitTime         time.Duration
}

// RetryHint is the wait the server suggests, in the range the retry loop can
// spend. A zero answer means the server published nothing usable.
func (s QueueStatus) RetryHint() time.Duration {
	hint := s.RetryAfter
	if hint <= 0 {
		hint = s.WaitTime
	}
	if hint <= 0 {
		return 0
	}
	if hint < time.Second {
		hint = time.Second
	}
	if hint > queueStatusMaxHint {
		hint = queueStatusMaxHint
	}
	return hint
}

// queueStatusMaxHint bounds what one status answer may ask this gateway to wait.
// The retry loop's own budget still applies on top of it.
const queueStatusMaxHint = 60 * time.Second

// QueueStatus reads the admission queue for one refused request.
func (c *Client) QueueStatus(ctx context.Context, requestSetID, sessionID, modelKey, queueType string) (QueueStatus, error) {
	if c == nil {
		return QueueStatus{}, fmt.Errorf("qoder client is nil")
	}
	if strings.TrimSpace(modelKey) == "" {
		return QueueStatus{}, errors.New("qoder queue status needs a model key")
	}
	creds, err := c.ensureAccessToken(ctx)
	if err != nil {
		return QueueStatus{}, err
	}
	fields, err := c.ensureRuntimeFields(ctx, creds)
	if err != nil {
		return QueueStatus{}, err
	}
	// The queue is served by the inference hosts, so it fails over the same way
	// a chat does: only a host that answered nothing at all is skipped.
	var lastErr error
	for _, host := range c.endpoints.inferenceHosts() {
		status, err := c.queueStatusOnce(ctx, creds, fields, host, requestSetID, sessionID, modelKey, queueType)
		if err == nil {
			return status, nil
		}
		lastErr = err
		if !errors.Is(err, errInferenceUnreachable) || ctx.Err() != nil {
			return QueueStatus{}, err
		}
	}
	return QueueStatus{}, lastErr
}

func (c *Client) queueStatusOnce(ctx context.Context, creds Credentials, fields RuntimeFields, host, requestSetID, sessionID, modelKey, queueType string) (QueueStatus, error) {
	query := url.Values{}
	query.Set("requestSetId", requestSetID)
	query.Set("modelKey", modelKey)
	if queueType != "" {
		query.Set("queueType", queueType)
	}
	rawURL := strings.TrimRight(host, "/") + queueStatusPath + "?" + query.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return QueueStatus{}, err
	}
	requestID, err := newUUID(c.entropy)
	if err != nil {
		return QueueStatus{}, err
	}
	if err := c.applyAuthHeaders(req, creds, fields, requestID, "", "", "", signPath(rawURL)); err != nil {
		return QueueStatus{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Request-ID", requestID)
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		req.Header.Set("X-Session-ID", sessionID)
	}

	resp, raw, err := httpclient.DoReadBody(c.control, req, 256<<10)
	if err != nil {
		if resp == nil && IsTransientTransport(err) {
			return QueueStatus{}, fmt.Errorf("%w: %w: %w", ErrAuthUnavailable, errInferenceUnreachable, err)
		}
		return QueueStatus{}, fmt.Errorf("%w: %w", ErrAuthUnavailable, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		// A host that does not serve the queue answers 404; another one may.
		return QueueStatus{}, fmt.Errorf("%w: queue status HTTP 404", errInferenceUnreachable)
	}
	if resp.StatusCode >= 400 {
		return QueueStatus{}, fmt.Errorf("qoder queue status: HTTP %d", resp.StatusCode)
	}
	status, ok := parseQueueStatus(raw)
	if !ok {
		return QueueStatus{}, errors.New("qoder queue status answered no queue fields")
	}
	return status, nil
}

// parseQueueStatus reads the queue fields out of an answer that may wrap them in
// data, result, message or body, and may quote them as a JSON string.
func parseQueueStatus(raw []byte) (QueueStatus, bool) {
	for _, document := range decodeQueueCandidates(raw, 0) {
		status := QueueStatus{Observed: true}
		if value, ok := queueBool(document, "isQueued"); ok {
			status.IsQueued = value
		}
		if value, ok := queueBool(document, "serviceAvailable"); ok {
			status.ServiceAvailable = value
		} else {
			status.ServiceAvailable = true
		}
		status.ModelKey, _ = queueString(document, "modelKey")
		status.QueueType, _ = queueString(document, "queueType")
		status.QueueCount, _ = queueInt(document, "queueCount")
		if seconds, ok := queueInt(document, "retryAfterSeconds"); ok && seconds > 0 {
			status.RetryAfter = time.Duration(seconds) * time.Second
		}
		if millis, ok := queueInt(document, "waitTime"); ok && millis > 0 {
			status.WaitTime = time.Duration(millis) * time.Millisecond
		}
		return status, true
	}
	return QueueStatus{}, false
}

// decodeQueueCandidates walks the wrappers the queue answer has been seen to use.
func decodeQueueCandidates(raw []byte, depth int) []map[string]any {
	if depth > 4 {
		return nil
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil {
		return nil
	}
	result := []map[string]any{document}
	for _, key := range []string{"data", "result", "message", "body"} {
		switch nested := document[key].(type) {
		case map[string]any:
			result = append(result, decodeQueueCandidates(mustQueueJSON(nested), depth+1)...)
		case string:
			if decoded := decodeQueueCandidates([]byte(nested), depth+1); len(decoded) > 0 {
				result = append(result, decoded...)
			}
		}
	}
	return result
}

func mustQueueJSON(value map[string]any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
}

func queueBool(document map[string]any, key string) (bool, bool) {
	if value, ok := document[key].(bool); ok {
		return value, true
	}
	return false, false
}

func queueString(document map[string]any, key string) (string, bool) {
	if value, ok := document[key].(string); ok {
		return value, true
	}
	return "", false
}

func queueInt(document map[string]any, key string) (int, bool) {
	switch value := document[key].(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	}
	return 0, false
}

// machineHostname is the value the client sends as Cosy-MachineHostname on the
// inference request class. It is read once: the host name does not change under
// a running process, and the header is on the hot path.
var (
	machineHostnameOnce sync.Once
	machineHostnameVal  string
)

// osHostname is indirect so tests do not depend on the host name.
var osHostname = os.Hostname

func machineHostname() string {
	machineHostnameOnce.Do(func() {
		if name, err := osHostname(); err == nil {
			machineHostnameVal = strings.TrimSpace(name)
		}
	})
	return machineHostnameVal
}
