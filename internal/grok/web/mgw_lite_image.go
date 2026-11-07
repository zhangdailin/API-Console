package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
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
	// Source names where the image was found: card, chunk or markdown.
	Source string
	// Diagnostics summarizes the turn: the event types, the output and chunk
	// fields and the start of the assistant text. It names what the gateway
	// sent without any credential.
	Diagnostics string
}

// grokDrawnImage reports an image grok.com itself hosts. A picture the chat
// linked from the web (a search result) is not a drawing of the prompt.
func grokDrawnImage(url string) bool {
	return url != "" && isGrokHostedAssetURL(url)
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
//
// The image is looked for where grok2api looks: a card_attachment output,
// then any finished image_chunk anywhere in a frame (also inside JSON-encoded
// strings), then a Markdown image in the assistant text.
func readMGWLiteImage(body io.Reader) (result mgwLiteResult, err error) {
	diag := newMGWLiteDiagnostics()
	defer func() { result.Diagnostics = diag.String() }()
	var text strings.Builder
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
		kind, _ := event["type"].(string)
		diag.observe(kind, event)
		if result.URL == "" {
			if url := firstFinishedImage(event); grokDrawnImage(url) {
				result.URL, result.Source = url, "chunk"
			}
		}
		switch kind {
		case "response.grok.output":
			output, _ := event["output"].(map[string]any)
			if streamError, _ := output["stream_error"].(map[string]any); streamError != nil {
				return result, mgwEventError(streamError, &result)
			}
			if url := mgwCardImage(output["card_attachment"]); grokDrawnImage(url) && result.URL == "" {
				result.URL, result.Source = url, "card"
			}
		case "response.chunk":
			chunk, _ := event["chunk"].(map[string]any)
			textField, _ := chunk["text"].(map[string]any)
			delta, _ := textField["text"].(string)
			text.WriteString(delta)
		case "response.output_text.delta":
			delta, _ := event["delta"].(string)
			text.WriteString(delta)
		case "error":
			return result, mgwEventError(event, &result)
		case "response.done":
			if url := firstMarkdownImage(text.String()); result.URL == "" && grokDrawnImage(url) {
				result.URL, result.Source = url, "markdown"
			}
			diag.text = text.String()
			response, _ := event["response"].(map[string]any)
			if status, _ := response["status"].(string); status != "" && status != "completed" && result.URL == "" {
				return result, fmt.Errorf("grok mgw response status %s", status)
			}
			return result, nil
		}
	}
	diag.text = text.String()
	if url := firstMarkdownImage(text.String()); result.URL == "" && grokDrawnImage(url) {
		result.URL, result.Source = url, "markdown"
	}
	if scanErr := scanner.Err(); scanErr != nil && result.URL == "" {
		return result, scanErr
	}
	return result, nil
}

// firstFinishedImage walks a decoded frame for a finished, unmoderated image
// chunk, decoding JSON carried as strings on the way.
func firstFinishedImage(value any) string {
	switch current := value.(type) {
	case map[string]any:
		for _, key := range []string{"image_chunk", "imageChunk"} {
			if chunk, ok := current[key].(map[string]any); ok {
				if url := finishedChunkURL(chunk); url != "" {
					return url
				}
			}
		}
		for _, nested := range current {
			if url := firstFinishedImage(nested); url != "" {
				return url
			}
		}
	case []any:
		for _, nested := range current {
			if url := firstFinishedImage(nested); url != "" {
				return url
			}
		}
	case string:
		trimmed := strings.TrimSpace(current)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var nested any
			if json.Unmarshal([]byte(trimmed), &nested) == nil {
				return firstFinishedImage(nested)
			}
		}
	}
	return ""
}

func finishedChunkURL(chunk map[string]any) string {
	if chunk == nil || isModeratedImageChunk(chunk) || interfaceToInt(chunk["progress"]) < 100 {
		return ""
	}
	for _, key := range []string{"imageUrl", "image_url", "url"} {
		if raw, _ := chunk[key].(string); strings.TrimSpace(raw) != "" {
			return normalizeGrokAssetURL(raw)
		}
	}
	return ""
}

// firstMarkdownImage returns the first ![...](url) image in text.
func firstMarkdownImage(text string) string {
	for {
		start := strings.Index(text, "](")
		if start < 0 {
			return ""
		}
		open := strings.LastIndex(text[:start], "![")
		rest := text[start+2:]
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return ""
		}
		if open >= 0 {
			if url := strings.TrimSpace(rest[:end]); url != "" {
				return normalizeGrokAssetURL(url)
			}
		}
		text = rest[end+1:]
	}
}

// mgwLiteDiagnostics records the shape of a turn for the log.
type mgwLiteDiagnostics struct {
	types      map[string]int
	outputKeys map[string]struct{}
	chunkKeys  map[string]struct{}
	text       string
}

func newMGWLiteDiagnostics() *mgwLiteDiagnostics {
	return &mgwLiteDiagnostics{types: map[string]int{}, outputKeys: map[string]struct{}{}, chunkKeys: map[string]struct{}{}}
}

func (d *mgwLiteDiagnostics) observe(kind string, event map[string]any) {
	d.types[kind]++
	if output, ok := event["output"].(map[string]any); ok {
		for key := range output {
			d.outputKeys[key] = struct{}{}
		}
	}
	if chunk, ok := event["chunk"].(map[string]any); ok {
		for key := range chunk {
			d.chunkKeys[key] = struct{}{}
		}
	}
}

func (d *mgwLiteDiagnostics) String() string {
	keys := func(set map[string]struct{}) string {
		out := make([]string, 0, len(set))
		for key := range set {
			out = append(out, key)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	types := make([]string, 0, len(d.types))
	for kind, n := range d.types {
		types = append(types, fmt.Sprintf("%s=%d", kind, n))
	}
	sort.Strings(types)
	text := d.text
	if r := []rune(text); len(r) > 160 {
		text = string(r[:160]) + "…"
	}
	return fmt.Sprintf("events[%s] output[%s] chunk[%s] text=%q", strings.Join(types, " "), keys(d.outputKeys), keys(d.chunkKeys), text)
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
