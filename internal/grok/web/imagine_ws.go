package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"encoding/json"

	"github.com/gorilla/websocket"

	"orchids-api/internal/httpclient"
	"orchids-api/internal/util"
)

const (
	defaultImagineWSURL     = "wss://grok.com/ws/imagine/listen"
	imagineWSConnectTimeout = 15 * time.Second
	imagineWSRoundTimeout   = 120 * time.Second
	// imagineWSFinalGrace bounds the wait for final image frames once every
	// generation of the round has reported "completed".
	imagineWSFinalGrace      = 15 * time.Second
	imagineWSInterRoundPause = 500 * time.Millisecond
)

type imagineWSEvent struct {
	Type    string
	ImageID string
	URL     string
	Blob    string
	Final   bool
}

// imagineWSSlot tracks one generation of a round. The preview (a frame with
// percentage_complete below 100) and the final image are kept apart so that a
// preview is never delivered as the result while the final frame can still come.
type imagineWSSlot struct {
	id          string
	url         string
	blob        string
	previewURL  string
	previewBlob string
	final       bool
	completed   bool
	moderated   bool
	emitted     bool
	progress    int
}

// imagineIncompleteError reports that every round ended with fewer usable
// images than the request asked for (moderated or never delivered upstream).
type imagineIncompleteError struct {
	got  int
	want int
}

func (e *imagineIncompleteError) Error() string {
	if e.got == 0 {
		return "no image generated"
	}
	return fmt.Sprintf("upstream returned only %d/%d usable images", e.got, e.want)
}

func isImageGenerationModel(modelID string) bool {
	spec, ok := ResolveModel(modelID)
	return ok && spec.IsImage && !isImageEditModel(spec.ID)
}

func isImageEditModel(modelID string) bool {
	return normalizeModelID(modelID) == "grok-imagine-image-edit"
}

func buildImagineWSResetMessage() map[string]any {
	return map[string]any{
		"type":      "conversation.item.create",
		"timestamp": time.Now().UnixMilli(),
		"item": map[string]any{
			"type":    "message",
			"content": []map[string]any{{"type": "reset"}},
		},
	}
}

func newImagineWSRequestID() string {
	if requestID := randomHex(16); requestID != "" {
		return requestID
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func buildImagineWSRequestMessage(requestID, prompt, aspectRatio string, nsfw bool, pro bool, generations int) map[string]any {
	return map[string]any{
		"type":      "conversation.item.create",
		"timestamp": time.Now().UnixMilli(),
		"item": map[string]any{
			"type": "message",
			"content": []map[string]any{{
				"requestId": requestID,
				"text":      strings.TrimSpace(prompt),
				"type":      "input_text",
				"properties": map[string]any{
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

// parseImagineWSImageID derives the image ID from an asset URL: the segment
// after "/images/" when present, otherwise the last path segment, both without
// the file extension.
func parseImagineWSImageID(rawURL string) string {
	u := strings.TrimSpace(rawURL)
	if cut := strings.IndexAny(u, "?#"); cut >= 0 {
		u = u[:cut]
	}
	u = strings.Trim(u, "/")
	if u == "" {
		return ""
	}
	name := u[strings.LastIndexByte(u, '/')+1:]
	if idx := strings.Index(strings.ToLower(u), "/images/"); idx >= 0 {
		name, _, _ = strings.Cut(u[idx+len("/images/"):], "/")
	}
	if dot := strings.IndexByte(name, '.'); dot > 0 {
		name = name[:dot]
	}
	return strings.TrimSpace(name)
}

func imagineFinalEvent(slot *imagineWSSlot) imagineWSEvent {
	if slot == nil {
		return imagineWSEvent{}
	}
	return imagineWSEvent{
		Type:    "image",
		ImageID: slot.id,
		URL:     slot.url,
		Blob:    slot.blob,
		Final:   true,
	}
}

func (h *Handler) streamImagineWSImages(ctx context.Context, sess *chatAccountSession, spec ModelSpec, prompt, aspectRatio string, n int, nsfw bool) (<-chan imagineWSEvent, <-chan error) {
	events := make(chan imagineWSEvent)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		// This goroutine is outside net/http's per-request recovery: a panic
		// here would take the whole process down.
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("Grok Imagine stream panicked", "panic", recovered)
				errs <- fmt.Errorf("grok imagine stream failed unexpectedly")
			}
		}()
		errs <- h.runImagineWSImages(ctx, sess, spec, prompt, aspectRatio, n, nsfw, events)
	}()
	return events, errs
}

// imagineWSMaxAccountSwitches bounds how many accounts one image request tries
// after an account's image allowance is refused (the edit path uses 3 too).
const imagineWSMaxAccountSwitches = 3

func (h *Handler) runImagineWSImages(ctx context.Context, sess *chatAccountSession, spec ModelSpec, prompt, aspectRatio string, n int, nsfw bool, events chan<- imagineWSEvent) error {
	if n < 1 {
		n = 1
	}
	client := h.currentClient()
	if client == nil {
		return fmt.Errorf("grok client not configured")
	}
	pro := imagineWSProModel(spec.ID)
	product := mediaProductFor(spec, "")
	collected, switches := 0, 0
	var excluded []int64
	maxRounds := max(n*3, 3)
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
		collected += roundFinals
		if wsErr, ok := errors.AsType[*imagineWSError](err); ok {
			// Which account and plan grok.com refused tells an account
			// allowance apart from a limit on the shared egress.
			var accountID int64
			var plan string
			if sess.acc != nil {
				accountID, plan = sess.acc.ID, sess.acc.Subscription
			}
			slog.Warn("Grok Imagine socket refused the request", "account_id", accountID, "plan", plan,
				"code", wsErr.code, "message", wsErr.message, "pro", pro, "switches", switches, "collected", collected)
		}
		if err != nil && isImagineAllowanceRefusal(err) {
			// This account's image allowance is spent; the account itself is
			// fine for everything else, so it is not cooled. Park the product
			// and try another account while no image has been delivered.
			h.noteMediaExhausted(ctx, sess.acc, product)
			if collected > 0 || switches >= imagineWSMaxAccountSwitches {
				// Answered like the selection fence: 429 with the reason.
				return &mediaUnavailableError{message: h.mediaUnavailableMessage(ctx, product), cause: err}
			}
			if sess.acc != nil && sess.acc.ID != 0 {
				excluded = append(excluded, sess.acc.ID)
			}
			next, switchErr := h.openChatAccountSessionForModelExcluding(ctx, excluded, spec)
			if switchErr != nil {
				return switchErr
			}
			sess.Close()
			sess.acc, sess.token, sess.poolCandidates, sess.release = next.acc, next.token, next.poolCandidates, next.release
			switches++
			round--
			continue
		}
		if err != nil {
			h.markAccountStatus(ctx, sess.acc, err)
			return err
		}
		if collected >= n {
			return nil
		}
		if !util.SleepWithContext(ctx, imagineWSInterRoundPause) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("no image generated")
}

func imagineWSString(msg map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := msg[key].(string); ok {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

// imagineWSFrameRequestID returns the request a frame says it belongs to, or
// "" when the frame does not say.
func imagineWSFrameRequestID(msg map[string]any) string {
	if id := imagineWSString(msg, "requestId", "request_id", "req_id", "reqId"); id != "" {
		return id
	}
	if item, ok := msg["item"].(map[string]any); ok {
		return imagineWSString(item, "requestId", "request_id")
	}
	return ""
}

// runImagineWSRound sends one generation request and reads frames until the
// round has `needed` usable images, every generation is terminal (completed or
// moderated), or the connection fails. It returns the number of final images
// it emitted.
//
// A gorilla/websocket read error is permanent: reading again after one,
// including after a deadline, panics once repeated. The loop therefore never
// re-reads after an error; deadlines are only ever moved before they expire.
func runImagineWSRound(ctx context.Context, conn *websocket.Conn, prompt, aspectRatio string, nsfw bool, pro bool, needed int, events chan<- imagineWSEvent) (int, error) {
	if conn == nil {
		return 0, fmt.Errorf("empty websocket connection")
	}
	needed = max(needed, 1)
	requestID := newImagineWSRequestID()
	if err := conn.WriteJSON(buildImagineWSResetMessage()); err != nil {
		return 0, err
	}
	if err := conn.WriteJSON(buildImagineWSRequestMessage(requestID, prompt, aspectRatio, nsfw, pro, needed)); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(imagineWSRoundTimeout)
	_ = conn.SetReadDeadline(deadline)
	// ReadMessage does not observe ctx; expiring the deadline unblocks it.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })
	defer stop()

	slots := map[string]*imagineWSSlot{}
	var order []*imagineWSSlot
	finals, terminal := 0, 0
	graceArmed := false
	emitFinal := func(slot *imagineWSSlot) bool {
		slot.emitted = true
		finals++
		return sendImagineEvent(ctx, events, imagineFinalEvent(slot))
	}
	// awaitingFinal reports a completed, unmoderated generation whose final
	// image frame has not arrived yet.
	awaitingFinal := func() bool {
		for _, slot := range order {
			if slot.completed && !slot.moderated && !slot.emitted {
				return true
			}
		}
		return false
	}
	for finals < needed {
		if err := ctx.Err(); err != nil {
			return finals, err
		}
		if terminal >= needed {
			if !awaitingFinal() {
				break
			}
			if !graceArmed {
				graceArmed = true
				if grace := time.Now().Add(imagineWSFinalGrace); grace.Before(deadline) {
					_ = conn.SetReadDeadline(grace)
					if err := ctx.Err(); err != nil {
						return finals, err
					}
				}
			}
		}
		mt, raw, err := conn.ReadMessage()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return finals, ctxErr
			}
			// The connection is gone. Deliver what is deliverable: a final frame
			// whose "completed" never came, then the last preview of a generation
			// upstream called completed without ever sending its final frame.
			for _, slot := range order {
				if slot.emitted || slot.moderated || finals >= needed {
					continue
				}
				if !slot.final {
					if !slot.completed || (slot.previewURL == "" && slot.previewBlob == "") {
						continue
					}
					slog.Warn("Grok Imagine delivered no final frame; using the last preview", "image_id", slot.id)
					slot.url, slot.blob = slot.previewURL, slot.previewBlob
				}
				if !emitFinal(slot) {
					return finals, ctx.Err()
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
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		frameType := strings.ToLower(imagineWSString(msg, "type"))
		if frameType == "error" {
			code := imagineWSString(msg, "err_code")
			text := imagineWSString(msg, "err_msg")
			if text == "" {
				text = string(raw)
			}
			return finals, &imagineWSError{code: code, message: text}
		}
		if frameType != "json" && frameType != "image" {
			continue
		}
		// A frame that names another request is not ours. Frames that name
		// none are accepted: each round has its own connection.
		if frameRequest := imagineWSFrameRequestID(msg); frameRequest != "" && frameRequest != requestID {
			continue
		}
		urlValue := imagineWSString(msg, "url")
		blobValue := imagineWSString(msg, "blob")
		urlID := parseImagineWSImageID(urlValue)
		imageID := imagineWSString(msg, "image_id", "job_id", "id")
		if urlID != "" && (imageID == "" || slots[urlID] != nil) {
			imageID = urlID
		}
		if imageID == "" {
			continue
		}
		slot := slots[imageID]
		if slot == nil {
			slot = &imagineWSSlot{id: imageID}
			slots[imageID] = slot
			order = append(order, slot)
		}
		if slot.emitted || slot.moderated {
			continue
		}
		if frameType == "image" {
			if urlValue == "" && blobValue == "" {
				continue
			}
			if percent, hasProgress := msg["percentage_complete"]; hasProgress && interfaceToInt(percent) < 100 {
				slot.previewURL, slot.previewBlob = urlValue, blobValue
				if progress := min(max(interfaceToInt(percent), 10), 99); progress > slot.progress {
					slot.progress = progress
					if !sendImagineEvent(ctx, events, imagineWSEvent{Type: "progress", ImageID: imageID, URL: urlValue, Blob: blobValue}) {
						return finals, ctx.Err()
					}
				}
				continue
			}
			slot.url, slot.blob, slot.final = urlValue, blobValue, true
			// The final frame may arrive after "completed".
			if slot.completed && !emitFinal(slot) {
				return finals, ctx.Err()
			}
			continue
		}
		switch imagineWSString(msg, "current_status") {
		case "start_stage":
			if slot.progress == 0 {
				slot.progress = 10
				if !sendImagineEvent(ctx, events, imagineWSEvent{Type: "progress", ImageID: imageID}) {
					return finals, ctx.Err()
				}
			}
		case "completed":
			if slot.completed {
				continue
			}
			slot.completed = true
			terminal++
			if moderated, _ := msg["moderated"].(bool); moderated {
				slot.moderated = true
				if !sendImagineEvent(ctx, events, imagineWSEvent{Type: "moderated", ImageID: imageID}) {
					return finals, ctx.Err()
				}
				continue
			}
			if !slot.final && urlValue != "" {
				slot.url, slot.blob, slot.final = urlValue, blobValue, true
			}
			if slot.final && !emitFinal(slot) {
				return finals, ctx.Err()
			}
		}
	}
	return finals, nil
}

// imagineWSError is an error frame grok.com sent on the Imagine socket. It is
// an upstream failure: its text carries the "upstream" marker so the shared
// writer classifies and logs it instead of answering 400 invalid_request.
type imagineWSError struct {
	code    string
	message string
}

func (e *imagineWSError) Error() string {
	if e.code == "" {
		return "grok imagine upstream error: " + e.message
	}
	return "grok imagine upstream error: " + e.code + ": " + e.message
}

// isImagineAllowanceRefusal reports the socket's refusal of this account's
// image allowance. grok.com answered both shapes on 2026-11-10, each to a
// different paid account: "rate_limit_exceeded: Image rate limit exceeded"
// and "usage_pool_exhausted: Usage limit exceeded".
func isImagineAllowanceRefusal(err error) bool {
	wsErr, ok := errors.AsType[*imagineWSError](err)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(wsErr.code)) {
	case "rate_limit_exceeded", "usage_pool_exhausted":
		return true
	}
	message := strings.ToLower(wsErr.message)
	return strings.Contains(message, "rate limit") || strings.Contains(message, "usage limit")
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
				if _, after, ok := strings.Cut(blob, ","); ok {
					return after, nil
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
