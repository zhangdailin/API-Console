package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// errMGWLiteUnavailable marks an account or deployment the MGW lite path
// cannot serve (no user id); the caller falls back to the REST request.
var errMGWLiteUnavailable = errors.New("grok mgw lite image unavailable")

// mgwLiteTimeout bounds one MGW drawing.
const mgwLiteTimeout = 90 * time.Second

// mgwLiteResult is what one MGW lite drawing produced.
type mgwLiteResult struct {
	// URL is the first finished image of the turn, the drawing of the prompt.
	URL string
	// LimitHit reports grok.com refusing the turn for a usage limit.
	LimitHit bool
}

// liteImageViaMGW draws one lite image through grok.com's MGW websocket
// gateway, the way grok2api does: a fast-mode session with image generation
// enabled, sent "Drawing: <prompt>". Images arrive as card_attachment outputs.
// opened reports that the turn reached grok.com; when it is false nothing was
// drawn and the caller may fall back to the REST request.
func (c *Client) liteImageViaMGW(ctx context.Context, userID, token, prompt string) (result mgwLiteResult, opened bool, err error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return mgwLiteResult{}, false, errMGWLiteUnavailable
	}
	message := strings.TrimSpace(prompt)
	if !strings.HasPrefix(strings.ToLower(message), "drawing:") {
		message = "Drawing: " + message
	}
	// One drawing; a socket that stops answering must not hold the request.
	ctx, cancel := context.WithTimeout(ctx, mgwLiteTimeout)
	defer cancel()
	transport := &mgwWebSocketTransport{client: c}
	resp, err := transport.Open(ctx, MGWChatRequest{UserID: userID, Model: "fast", Prompt: message, Token: token, ImageGeneration: true})
	if err != nil {
		return mgwLiteResult{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mgwLiteResult{}, false, fmt.Errorf("grok mgw handshake refused with status %d", resp.StatusCode)
	}
	result, err = readMGWLiteImage(resp.Body)
	return result, true, err
}

// readMGWLiteImage reads MGW frames until the turn ends and returns its first
// finished, unmoderated image. A usage-limit error is reported as LimitHit.
func readMGWLiteImage(body io.Reader) (mgwLiteResult, error) {
	var result mgwLiteResult
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), mgwMaxFrameBytes+1)
	for scanner.Scan() {
		var frame struct {
			Event map[string]any `json:"event"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil || frame.Event == nil {
			continue
		}
		event := frame.Event
		switch kind, _ := event["type"].(string); kind {
		case "response.grok.output":
			output, _ := event["output"].(map[string]any)
			if streamError, _ := output["stream_error"].(map[string]any); streamError != nil {
				return result, mgwEventError(streamError, &result)
			}
			if url := mgwCardImage(output["card_attachment"]); url != "" && result.URL == "" {
				result.URL = url
			}
		case "error":
			return result, mgwEventError(event, &result)
		case "response.done":
			response, _ := event["response"].(map[string]any)
			if status, _ := response["status"].(string); status != "" && status != "completed" && result.URL == "" {
				return result, fmt.Errorf("grok mgw response status %s", status)
			}
			return result, nil
		}
	}
	if err := scanner.Err(); err != nil && result.URL == "" {
		return result, err
	}
	return result, nil
}

// mgwCardImage returns the finished image of a card_attachment (an object or
// a list of them): an image_chunk at 100% that grok.com did not moderate.
func mgwCardImage(value any) string {
	if list, ok := value.([]any); ok {
		for _, item := range list {
			if url := mgwCardImage(item); url != "" {
				return url
			}
		}
		return ""
	}
	card, _ := value.(map[string]any)
	if card == nil {
		return ""
	}
	data, _ := parseGrokJSONData(card["jsonData"]).(map[string]any)
	if data == nil {
		data = card
	}
	chunk, _ := data["image_chunk"].(map[string]any)
	if chunk == nil {
		chunk, _ = data["imageChunk"].(map[string]any)
	}
	if chunk == nil || isModeratedImageChunk(chunk) || interfaceToInt(chunk["progress"]) < 100 {
		return ""
	}
	for _, key := range []string{"imageUrl", "image_url"} {
		if raw, _ := chunk[key].(string); strings.TrimSpace(raw) != "" {
			return normalizeGrokAssetURL(raw)
		}
	}
	return ""
}

// mgwEventError turns an MGW error payload into an error, flagging a usage
// limit so the caller can move to another account.
func mgwEventError(payload map[string]any, result *mgwLiteResult) error {
	raw, _ := json.Marshal(payload)
	text := strings.ToLower(string(raw))
	if strings.Contains(text, "limit") || strings.Contains(text, "quota") || strings.Contains(text, "too many") {
		result.LimitHit = true
	}
	return fmt.Errorf("grok mgw error: %s", upstreamErrorValueMessage(payload))
}
