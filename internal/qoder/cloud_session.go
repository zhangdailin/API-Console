package qoder

// The cloud session transport.
//
// The channel has always emulated the inference endpoint
// (/algo/api/v2/service/pro/sse/agent_chat_generation) with a COSY-signed body.
// The desktop client does not call that endpoint: it opens a server-side session,
// posts events into it, and reads an event stream. The inference endpoint puts
// its callers into a low-priority admission queue (10605 / queueType p3|p4), so
// the same account answers the IDE immediately while a queued request waits for
// the per-minute window.
//
// This file implements the loop the IDE uses — environment, agent, session, one
// user.message, event stream — so the channel can be measured and, later, served
// through it. It is a separate transport: nothing here changes the inference path
// until a caller selects it.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/upstream"
)

// DefaultCloudBaseURL is the session API the desktop client uses.
const DefaultCloudBaseURL = "https://api.qoder.com/api/v1/cloud"

// cloudTargetsSetting caches the environment and agent created for this
// deployment, so a restart does not orphan a new pair on every run.
const cloudTargetsSetting = "qoder_cloud_targets"

// cloudBaseURL is the session API base. It is a variable so the operator probe
// and tests can point it elsewhere; production leaves the default.
var cloudBaseURL = DefaultCloudBaseURL

// SetCloudBaseURL overrides the session API base for this process.
func SetCloudBaseURL(base string) {
	if trimmed := strings.TrimRight(strings.TrimSpace(base), "/"); trimmed != "" {
		cloudBaseURL = trimmed
	}
}

// SettingsStore persists deployment-scoped settings. *store.Store satisfies it;
// the channel keeps its own narrow interface so it does not depend on the store.
type SettingsStore interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// CloudTargets is the environment and agent a session is created in.
type CloudTargets struct {
	EnvironmentID string `json:"environment_id"`
	AgentID       string `json:"agent_id"`
}

// CloudTurnResult reports what one session turn produced.
type CloudTurnResult struct {
	SessionID string
	Events    int
	TextBytes int
	LastType  string
}

func (c *Client) cloudAccessToken() string {
	return strings.TrimSpace(c.currentCredentials().AccessToken)
}

// cloudRequest performs one session-API call. The session API authenticates with
// the account's OAuth bearer token; the COSY signature and the private body
// encoding belong to the inference endpoint and are not used here.
func (c *Client) cloudRequest(ctx context.Context, method, path string, body any, accept string, stream bool) (*http.Response, error) {
	token := c.cloudAccessToken()
	if token == "" {
		return nil, fmt.Errorf("qoder cloud session needs an access token")
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode qoder cloud request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, cloudBaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Qoder-Remote-Api-Version", "2")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	client := c.control
	if stream {
		client = c.stream
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("qoder cloud %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return resp, nil
}

func (c *Client) cloudJSON(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.cloudRequest(ctx, method, path, body, "application/json", false)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode qoder cloud %s %s: %w", method, path, err)
	}
	return nil
}

// CreateCloudEnvironment creates the runtime environment a session runs in.
func (c *Client) CreateCloudEnvironment(ctx context.Context, name string) (string, error) {
	body := map[string]any{
		"name": strings.TrimSpace(name),
		"config": map[string]any{
			"type":       "cloud",
			"networking": "open",
			"packages":   map[string]any{"apt": []any{}, "npm": []any{}, "pip": []any{}},
		},
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.cloudJSON(ctx, http.MethodPost, "/environments", body, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.ID) == "" {
		return "", fmt.Errorf("qoder cloud environment response did not include an id")
	}
	return out.ID, nil
}

// CreateCloudAgent creates the agent a session belongs to.
func (c *Client) CreateCloudAgent(ctx context.Context, name string) (string, error) {
	body := map[string]any{
		"name":     strings.TrimSpace(name),
		"model":    "auto",
		"metadata": map[string]any{"created_by": "orchids"},
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.cloudJSON(ctx, http.MethodPost, "/agents", body, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.ID) == "" {
		return "", fmt.Errorf("qoder cloud agent response did not include an id")
	}
	return out.ID, nil
}

// CreateCloudSession opens a session in the given environment.
func (c *Client) CreateCloudSession(ctx context.Context, agentID, environmentID string) (string, error) {
	body := map[string]any{"agent": strings.TrimSpace(agentID), "environment_id": strings.TrimSpace(environmentID)}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.cloudJSON(ctx, http.MethodPost, "/sessions", body, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.ID) == "" {
		return "", fmt.Errorf("qoder cloud session response did not include an id")
	}
	return out.ID, nil
}

// PostCloudEvents posts one batch of events into a session.
func (c *Client) PostCloudEvents(ctx context.Context, sessionID string, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	body := map[string]any{"events": events}
	path := "/sessions/" + url.PathEscape(sessionID) + "/events"
	return c.cloudJSON(ctx, http.MethodPost, path, body, nil)
}

// EnsureCloudTargets returns the cached environment and agent, creating them on
// first use. They belong to this deployment, not to a single request.
func (c *Client) EnsureCloudTargets(ctx context.Context) (CloudTargets, error) {
	if c.settings != nil {
		if raw, err := c.settings.GetSetting(ctx, cloudTargetsSetting); err == nil && strings.TrimSpace(raw) != "" {
			var cached CloudTargets
			if json.Unmarshal([]byte(raw), &cached) == nil &&
				cached.AgentID != "" && cached.EnvironmentID != "" {
				return cached, nil
			}
		}
	}
	name := "orchids-" + time.Now().UTC().Format("20060102")
	environmentID, err := c.CreateCloudEnvironment(ctx, name)
	if err != nil {
		return CloudTargets{}, err
	}
	agentID, err := c.CreateCloudAgent(ctx, name)
	if err != nil {
		return CloudTargets{}, err
	}
	targets := CloudTargets{EnvironmentID: environmentID, AgentID: agentID}
	if c.settings != nil {
		if raw, err := json.Marshal(targets); err == nil {
			_ = c.settings.SetSetting(ctx, cloudTargetsSetting, string(raw))
		}
	}
	return targets, nil
}

// cloudEvent is one decoded server-sent event from the session stream.
type cloudEvent struct {
	Type string
	Seq  int64
	Data map[string]any
}

// cloudEventText extracts the assistant text an event carries. The stream has
// been observed to carry text two ways — a delta string, and a message whose
// content is a string or a list of parts — so the caller reconciles against what
// it already emitted instead of trusting the shape.
func cloudEventText(data map[string]any) string {
	if data == nil {
		return ""
	}
	if delta, ok := data["delta"].(string); ok && delta != "" {
		return delta
	}
	return cloudContentText(data["content"])
}

func cloudContentText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, part := range typed {
			entry, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := entry["text"].(string); ok {
				builder.WriteString(text)
			}
		}
		return builder.String()
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return text
		}
		if nested, ok := typed["content"]; ok {
			return cloudContentText(nested)
		}
	}
	return ""
}

// readCloudEventStream consumes the session event stream and dispatches each
// event until the stream ends, the context ends, or the session reports idle.
func (c *Client) readCloudEventStream(ctx context.Context, sessionID string, afterSeq int64, onEvent func(cloudEvent) error) error {
	path := "/sessions/" + url.PathEscape(sessionID) + "/events/stream"
	if afterSeq > 0 {
		path += "?from_sequence_num=" + strconv.FormatInt(afterSeq, 10)
	}
	resp, err := c.cloudRequest(ctx, http.MethodGet, path, nil, "text/event-stream", true)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return fmt.Errorf("qoder cloud stream answered %q instead of an event stream", resp.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var eventType string
	var payload strings.Builder
	dispatch := func() error {
		raw := strings.TrimSpace(payload.String())
		payload.Reset()
		current := eventType
		eventType = ""
		if raw == "" {
			return nil
		}
		event := cloudEvent{Type: current, Data: map[string]any{}}
		if err := json.Unmarshal([]byte(raw), &event.Data); err != nil {
			// An unparsable frame is skipped rather than ending the turn.
			return nil
		}
		if name, ok := event.Data["type"].(string); ok && name != "" {
			event.Type = name
		}
		if seq, ok := event.Data["sequence_num"].(float64); ok {
			event.Seq = int64(seq)
		}
		if event.Type == "" {
			event.Type = "message"
		}
		return onEvent(event)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if payload.Len() > 0 {
				payload.WriteByte('\n')
			}
			payload.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return dispatch()
}

// cloudStreamResult is what the reader goroutine reports back.
type cloudStreamResult struct {
	events  int
	last    string
	emitted int
}

// RunCloudTurn runs one complete turn through the session API: it reuses the
// deployment's environment and agent, opens a session, streams it, posts the user
// message, and emits assistant text as upstream deltas until the session reports
// it is idle or the stream ends.
func (c *Client) RunCloudTurn(ctx context.Context, prompt string, onMessage func(upstream.SSEMessage)) (CloudTurnResult, error) {
	result := CloudTurnResult{}
	targets, err := c.EnsureCloudTargets(ctx)
	if err != nil {
		return result, err
	}
	sessionID, err := c.CreateCloudSession(ctx, targets.AgentID, targets.EnvironmentID)
	if err != nil {
		return result, err
	}
	result.SessionID = sessionID

	// The stream is opened before the message is posted: an event posted into a
	// session with no reader would race the answer.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type streamOutcome struct {
		summary cloudStreamResult
		err     error
	}
	done := make(chan streamOutcome, 1)
	go func() {
		summary := cloudStreamResult{}
		var full strings.Builder
		var sawText bool
		err := c.readCloudEventStream(streamCtx, sessionID, 0, func(event cloudEvent) error {
			summary.events++
			summary.last = event.Type
			text := cloudEventText(event.Data)
			if text == "" {
				if event.Type == "session.status_idle" || event.Type == "terminated" {
					cancel()
				}
				return nil
			}
			// Two shapes are reconciled here: a growing message (extend the
			// prefix) and a bare delta (append it).
			var delta string
			switch {
			case summary.emitted == 0:
				delta = text
				full.WriteString(text)
			case strings.HasPrefix(text, full.String()) && len(text) > full.Len():
				delta = text[full.Len():]
				full.Reset()
				full.WriteString(text)
			case len(text) <= full.Len() && strings.HasPrefix(full.String(), text):
				delta = ""
			default:
				delta = text
				full.WriteString(text)
			}
			summary.emitted = full.Len()
			if delta != "" {
				upstream.EmitTextDelta(onMessage, delta, &sawText)
			}
			return nil
		})
		done <- streamOutcome{summary: summary, err: err}
	}()

	postCtx, cancelPost := context.WithTimeout(ctx, 30*time.Second)
	err = c.PostCloudEvents(postCtx, sessionID, []map[string]any{{
		"type":    "user.message",
		"content": strings.TrimSpace(prompt),
	}})
	cancelPost()
	if err != nil {
		cancel()
		<-done
		return result, err
	}

	select {
	case outcome := <-done:
		result.Events = outcome.summary.events
		result.TextBytes = outcome.summary.emitted
		result.LastType = outcome.summary.last
		if outcome.err != nil && ctx.Err() == nil {
			return result, outcome.err
		}
	case <-ctx.Done():
		return result, ctx.Err()
	}
	return result, nil
}
