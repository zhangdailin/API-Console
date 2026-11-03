package web

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"encoding/json"

	"github.com/gorilla/websocket"

	"orchids-api/internal/httpclient"
	"orchids-api/internal/util"
)

const (
	defaultImagineWSURL      = "wss://grok.com/ws/imagine/listen"
	imagineWSConnectTimeout  = 15 * time.Second
	imagineWSRoundTimeout    = 120 * time.Second
	imagineWSReadTimeout     = 15 * time.Second
	imagineWSInterRoundPause = 500 * time.Millisecond
)

type imagineWSEvent struct {
	Type    string
	ImageID string
	URL     string
	Blob    string
	Final   bool
}

type imagineWSSlot struct {
	id       string
	lastURL  string
	lastBlob string
	done     bool
	progress int
}

func isImageGenerationModel(modelID string) bool {
	spec, ok := ResolveModel(modelID)
	return ok && spec.IsImage && !isImageEditModel(spec.ID)
}

func isImageEditModel(modelID string) bool {
	return normalizeModelID(modelID) == "grok-imagine-image-edit"
}

func buildImagineWSResetMessage() map[string]interface{} {
	return map[string]interface{}{
		"type":      "conversation.item.create",
		"timestamp": time.Now().UnixMilli(),
		"item": map[string]interface{}{
			"type":    "message",
			"content": []map[string]interface{}{{"type": "reset"}},
		},
	}
}

func buildImagineWSRequestMessage(prompt, aspectRatio string, nsfw bool, pro bool, generations int) map[string]interface{} {
	requestID := randomHex(16)
	if requestID == "" {
		requestID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return map[string]interface{}{
		"type":      "conversation.item.create",
		"timestamp": time.Now().UnixMilli(),
		"item": map[string]interface{}{
			"type": "message",
			"content": []map[string]interface{}{{
				"requestId": requestID,
				"text":      strings.TrimSpace(prompt),
				"type":      "input_text",
				"properties": map[string]interface{}{
					"section_count":       0,
					"is_kids_mode":        false,
					"enable_nsfw":         nsfw,
					"skip_upsampler":      false,
					"enable_side_by_side": true,
					"is_initial":          false,
					"aspect_ratio":        resolveAspectRatio(aspectRatio),
					"enable_pro":          pro,
					"num_generations":     max(generations, 1),
				},
			}},
		},
	}
}

func (c *Client) imagineWSHeaders(ctx context.Context, token string) http.Header {
	// The handshake is a signed request like any other web-plane call: grok2api
	// resolves x-statsig-id per method+path, and the WebSocket upgrade is the same
	// document origin as /imagine.
	h := c.headersFor(ctx, token, http.MethodGet, c.baseURL()+"/ws/imagine/listen")
	h.Del("Content-Type")
	h.Set("Origin", "https://grok.com")
	h.Set("Referer", "https://grok.com/imagine")
	h.Set("Sec-Fetch-Dest", "websocket")
	h.Set("Sec-Fetch-Mode", "websocket")
	return h
}

func (c *Client) dialImagineWS(ctx context.Context, token string) (*websocket.Conn, *http.Response, error) {
	if c == nil {
		return nil, nil, fmt.Errorf("grok client not configured")
	}
	proxyFunc := httpclient.ProxyFuncFromConfig(c.cfg)
	dialer := websocket.Dialer{
		HandshakeTimeout: imagineWSConnectTimeout,
		Proxy:            proxyFunc,
	}
	return dialer.DialContext(ctx, defaultImagineWSURL, c.imagineWSHeaders(ctx, token))
}

func parseImagineWSImageID(rawURL string) string {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return ""
	}
	lower := strings.ToLower(u)
	marker := "/images/"
	idx := strings.Index(lower, marker)
	if idx < 0 {
		return ""
	}
	rest := u[idx+len(marker):]
	for i, r := range rest {
		if r == '.' || r == '/' || r == '?' || r == '#' {
			return strings.TrimSpace(rest[:i])
		}
	}
	return strings.TrimSpace(rest)
}

func imagineFinalEvent(slot *imagineWSSlot) imagineWSEvent {
	if slot == nil {
		return imagineWSEvent{}
	}
	return imagineWSEvent{
		Type:    "image",
		ImageID: slot.id,
		URL:     slot.lastURL,
		Blob:    slot.lastBlob,
		Final:   true,
	}
}

func (h *Handler) streamImagineWSImages(ctx context.Context, sess *chatAccountSession, prompt, aspectRatio string, n int, nsfw bool, pro bool) (<-chan imagineWSEvent, <-chan error) {
	events := make(chan imagineWSEvent)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		errs <- h.runImagineWSImages(ctx, sess, prompt, aspectRatio, n, nsfw, pro, events)
	}()
	return events, errs
}

func (h *Handler) runImagineWSImages(ctx context.Context, sess *chatAccountSession, prompt, aspectRatio string, n int, nsfw bool, pro bool, events chan<- imagineWSEvent) error {
	if n < 1 {
		n = 1
	}
	client := h.currentClient()
	if client == nil {
		return fmt.Errorf("grok client not configured")
	}
	collected := 0
	maxRounds := n * 3
	if maxRounds < 3 {
		maxRounds = 3
	}
	for round := 0; round < maxRounds && collected < n; round++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		roundCtx, cancel := context.WithTimeout(ctx, imagineWSRoundTimeout)
		conn, resp, err := client.dialImagineWS(roundCtx, sess.token)
		if err != nil {
			cancel()
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			dialErr := fmt.Errorf("imagine websocket dial failed status=%d: %w", status, err)
			h.markAccountStatus(ctx, sess.acc, dialErr)
			return dialErr
		}
		roundFinals, err := runImagineWSRound(roundCtx, conn, prompt, aspectRatio, nsfw, pro, n-collected, events)
		_ = conn.Close()
		cancel()
		if err != nil {
			h.markAccountStatus(ctx, sess.acc, err)
			return err
		}
		collected += roundFinals
		if collected >= n {
			return nil
		}
		if !util.SleepWithContext(ctx, imagineWSInterRoundPause) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("no image generated")
}

func runImagineWSRound(ctx context.Context, conn *websocket.Conn, prompt, aspectRatio string, nsfw bool, pro bool, needed int, events chan<- imagineWSEvent) (int, error) {
	if conn == nil {
		return 0, fmt.Errorf("empty websocket connection")
	}
	if err := conn.WriteJSON(buildImagineWSResetMessage()); err != nil {
		return 0, err
	}
	if err := conn.WriteJSON(buildImagineWSRequestMessage(prompt, aspectRatio, nsfw, pro, needed)); err != nil {
		return 0, err
	}
	slots := map[string]*imagineWSSlot{}
	finals := 0
	deadline := time.Now().Add(imagineWSRoundTimeout)
	for finals < needed {
		if err := ctx.Err(); err != nil {
			return finals, err
		}
		readDeadline := time.Now().Add(imagineWSReadTimeout)
		if readDeadline.After(deadline) {
			readDeadline = deadline
		}
		_ = conn.SetReadDeadline(readDeadline)
		mt, raw, err := conn.ReadMessage()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() && time.Now().Before(deadline) {
				continue
			}
			for _, slot := range slots {
				if !slot.done && strings.TrimSpace(slot.lastBlob) != "" {
					slot.done = true
					finals++
					if !sendImagineEvent(ctx, events, imagineFinalEvent(slot)) {
						return finals, ctx.Err()
					}
				}
			}
			if finals > 0 {
				return finals, nil
			}
			return finals, err
		}
		if mt != websocket.TextMessage {
			continue
		}
		var msg map[string]interface{}
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(fmt.Sprint(msg["type"]))) {
		case "json":
			status := strings.TrimSpace(fmt.Sprint(msg["current_status"]))
			imageID := strings.TrimSpace(fmt.Sprint(msg["image_id"]))
			if imageID == "" {
				imageID = strings.TrimSpace(fmt.Sprint(msg["job_id"]))
			}
			if imageID == "" {
				continue
			}
			switch status {
			case "start_stage":
				slot := &imagineWSSlot{
					id:       imageID,
					progress: 10,
				}
				slots[imageID] = slot
				if !sendImagineEvent(ctx, events, imagineWSEvent{Type: "progress", ImageID: imageID}) {
					return finals, ctx.Err()
				}
			case "completed":
				slot := slots[imageID]
				if slot == nil || slot.done {
					continue
				}
				slot.done = true
				if moderated, _ := msg["moderated"].(bool); moderated {
					if !sendImagineEvent(ctx, events, imagineWSEvent{Type: "moderated", ImageID: imageID}) {
						return finals, ctx.Err()
					}
					continue
				}
				finals++
				if !sendImagineEvent(ctx, events, imagineFinalEvent(slot)) {
					return finals, ctx.Err()
				}
			}
		case "image":
			urlValue := strings.TrimSpace(fmt.Sprint(msg["url"]))
			imageID := parseImagineWSImageID(urlValue)
			if imageID == "" {
				continue
			}
			slot := slots[imageID]
			if slot == nil || slot.done {
				continue
			}
			slot.lastURL = urlValue
			slot.lastBlob = strings.TrimSpace(fmt.Sprint(msg["blob"]))
			progress := min(max(interfaceToInt(msg["percentage_complete"]), 10), 99)
			if progress > slot.progress {
				slot.progress = progress
				if !sendImagineEvent(ctx, events, imagineWSEvent{Type: "progress", ImageID: imageID, URL: slot.lastURL, Blob: slot.lastBlob}) {
					return finals, ctx.Err()
				}
			}
		case "error":
			code := strings.TrimSpace(fmt.Sprint(msg["err_code"]))
			text := strings.TrimSpace(fmt.Sprint(msg["err_msg"]))
			if text == "" {
				text = string(raw)
			}
			if code != "" {
				text = code + ": " + text
			}
			return finals, fmt.Errorf("imagine websocket error: %s", text)
		}
	}
	return finals, nil
}

func sendImagineEvent(ctx context.Context, events chan<- imagineWSEvent, ev imagineWSEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case events <- ev:
		return true
	}
}

func (h *Handler) imagineImageOutputValue(ctx context.Context, token string, ev imagineWSEvent, format string) (string, error) {
	blob := strings.TrimSpace(ev.Blob)
	if blob != "" {
		if strings.HasPrefix(blob, "data:image/") {
			if normalizeImageResponseFormat(format) == "b64_json" {
				if idx := strings.Index(blob, ","); idx >= 0 {
					return blob[idx+1:], nil
				}
			}
			name, err := h.cacheImageDataURI(blob, ev.URL)
			if err == nil && name != "" {
				return "/grok/v1/files/image/" + name, nil
			}
		} else if raw, err := base64.StdEncoding.DecodeString(blob); err == nil && len(raw) > 0 {
			if normalizeImageResponseFormat(format) == "b64_json" {
				return blob, nil
			}
			name, cacheErr := h.cacheMediaBytes(firstNonEmpty(ev.URL, ev.ImageID), "image", raw, mimeFromFilename(ev.URL))
			if cacheErr == nil && name != "" {
				return "/grok/v1/files/image/" + name, nil
			}
		}
	}
	return h.imageOutputValue(ctx, token, ev.URL, format)
}

func (h *Handler) cacheImageDataURI(dataURI, rawURL string) (string, error) {
	_, b64, mime, err := parseDataURI(dataURI)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	return h.cacheMediaBytes(firstNonEmpty(rawURL, dataURI), "image", raw, mime)
}
