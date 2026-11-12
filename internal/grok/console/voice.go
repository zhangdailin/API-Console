package console

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"orchids-api/internal/httpclient"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/pricing"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

const (
	defaultVoiceModel = "grok-voice-latest"
	defaultSTTModel   = "grok-stt"
	maxAudioUpload    = 64 << 20
	maxAudioResponse  = 64 << 20
	maxTTSCharacters  = 15000
)

// qualify maps a voice model name to its Console route: voice models exist
// only on Console, so the bare name is accepted too.
func qualify(model, fallback string) string {
	model = strings.ToLower(strings.TrimSpace(util.FirstNonEmpty(model, fallback)))
	if IsConsoleModelID(model) {
		return model
	}
	return RoutePrefix + model
}

// openAIAudioCodec maps an OpenAI response_format to a Console codec.
func openAIAudioCodec(format string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "mp3":
		return "mp3", true
	case "opus", "ogg":
		return "opus", true
	case "aac":
		return "aac", true
	case "flac":
		return "flac", true
	case "wav", "wave":
		return "wav", true
	case "pcm", "pcm16":
		return "pcm", true
	}
	return "", false
}

// HandleTTS serves POST /tts with Console's own request shape
// ({"text","language","voice_id",...}); model only selects the route.
func (h *Handler) HandleTTS(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "the request body must be a JSON object")
		return
	}
	model, _ := payload["model"].(string)
	delete(payload, "model")
	h.synthesize(w, r, qualify(model, defaultVoiceModel), payload)
}

// HandleAudioSpeech serves the OpenAI-compatible POST /audio/speech.
func (h *Handler) HandleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req struct {
		Model          string   `json:"model"`
		Input          string   `json:"input"`
		Voice          string   `json:"voice"`
		VoiceID        string   `json:"voice_id"`
		Language       string   `json:"language"`
		ResponseFormat string   `json:"response_format"`
		Speed          *float64 `json:"speed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "the request body must be a JSON object")
		return
	}
	codec, ok := openAIAudioCodec(req.ResponseFormat)
	if !ok {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "response_format is not supported")
		return
	}
	payload := map[string]any{
		"text":          req.Input,
		"language":      util.FirstNonEmpty(strings.TrimSpace(req.Language), "en"),
		"output_format": map[string]any{"codec": codec},
	}
	if voice := util.FirstNonEmpty(strings.TrimSpace(req.VoiceID), strings.TrimSpace(req.Voice)); voice != "" {
		payload["voice_id"] = voice
	}
	if req.Speed != nil {
		if *req.Speed < 0.25 || *req.Speed > 4 {
			httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "speed must be between 0.25 and 4")
			return
		}
		payload["speed"] = *req.Speed
	}
	h.synthesize(w, r, qualify(req.Model, defaultVoiceModel), payload)
}

func (h *Handler) synthesize(w http.ResponseWriter, r *http.Request, model string, payload map[string]any) {
	text, _ := payload["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "text (input) must not be empty")
		return
	}
	if len([]rune(text)) > maxTTSCharacters {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "text is limited to 15000 characters")
		return
	}
	if language, _ := payload["language"].(string); strings.TrimSpace(language) == "" {
		payload["language"] = "en"
	}
	row, ok := h.resolveForRequest(w, r, model, store.CapabilityTTS)
	if !ok {
		return
	}
	body, _ := json.Marshal(payload)
	resp, sess, err := h.Call(r.Context(), row, func(sess *Session) (*http.Response, error) {
		return h.consoleClient().Do(r.Context(), sess.Token, http.MethodPost, "/tts", body, "application/json", "*/*")
	})
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer sess.Close()
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAudioResponse+1))
	if err != nil || len(data) > maxAudioResponse {
		httpserver.WriteErrorCode(w, http.StatusBadGateway, "upstream_error", "the Console speech response could not be read")
		return
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "application/json") {
		// A JSON envelope carries the audio base64-encoded.
		var envelope struct {
			Audio       string `json:"audio"`
			ContentType string `json:"content_type"`
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.Audio != "" {
			if audio, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(envelope.Audio)); decodeErr == nil {
				data, contentType = audio, util.FirstNonEmpty(envelope.ContentType, "audio/mpeg")
			}
		}
	}
	w.Header().Set("Content-Type", util.FirstNonEmpty(contentType, "audio/mpeg"))
	_, _ = w.Write(data)
	cost, priced := pricing.EstimateTTSCost(text)
	h.recordMedia(r.Context(), sess.Account, row, cost, priced, map[string]any{"endpoint": "tts", "characters": len([]rune(text))})
}

// HandleTTSVoices serves GET /tts/voices from any Console account that has a
// voice route.
func (h *Handler) HandleTTSVoices(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodGet) {
		return
	}
	row, ok := h.resolveForRequest(w, r, qualify(r.URL.Query().Get("model"), defaultVoiceModel), store.CapabilityTTS)
	if !ok {
		return
	}
	resp, sess, err := h.Call(r.Context(), row, func(sess *Session) (*http.Response, error) {
		return h.consoleClient().Do(r.Context(), sess.Token, http.MethodGet, "/tts/voices", nil, "", "application/json")
	})
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer sess.Close()
	relay(w, resp)
}

// sttForm is a parsed transcription upload.
type sttForm struct {
	model  string
	fields map[string][]string
	file   []byte
	name   string
}

// readSTTForm parses a multipart transcription request. drop names the
// client fields Console does not take.
func readSTTForm(w http.ResponseWriter, r *http.Request, drop map[string]bool) (*sttForm, bool) {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		httpserver.WriteErrorCode(w, http.StatusUnsupportedMediaType, "invalid_request_error", "transcription takes multipart/form-data with a file field")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAudioUpload)
	if err := r.ParseMultipartForm(maxAudioUpload); err != nil {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "the multipart body could not be read")
		return nil, false
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	form := &sttForm{fields: map[string][]string{}}
	for name, values := range r.MultipartForm.Value {
		switch {
		case name == "model":
			form.model = strings.TrimSpace(util.FirstNonEmpty(values...))
		case !drop[name]:
			form.fields[name] = values
		}
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		if len(form.fields["url"]) == 0 {
			httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "file is required")
			return nil, false
		}
		return form, true
	}
	opened, err := files[0].Open()
	if err != nil {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "the uploaded file could not be read")
		return nil, false
	}
	defer opened.Close()
	if form.file, err = io.ReadAll(io.LimitReader(opened, maxAudioUpload)); err != nil {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "the uploaded file could not be read")
		return nil, false
	}
	form.name = util.FirstNonEmpty(files[0].Filename, "audio.bin")
	return form, true
}

func (f *sttForm) encode(upstreamModel string) ([]byte, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, values := range f.fields {
		for _, value := range values {
			if err := writer.WriteField(name, value); err != nil {
				return nil, "", err
			}
		}
	}
	if err := writer.WriteField("model", upstreamModel); err != nil {
		return nil, "", err
	}
	if f.file != nil {
		part, err := writer.CreateFormFile("file", f.name)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(f.file); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func (h *Handler) transcribe(w http.ResponseWriter, r *http.Request, form *sttForm) ([]byte, bool) {
	row, ok := h.resolveForRequest(w, r, qualify(form.model, defaultSTTModel), store.CapabilitySTT)
	if !ok {
		return nil, false
	}
	body, contentType, err := form.encode(row.UpstreamModel)
	if err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid request")
		return nil, false
	}
	resp, sess, err := h.Call(r.Context(), row, func(sess *Session) (*http.Response, error) {
		return h.consoleClient().Do(r.Context(), sess.Token, http.MethodPost, "/stt", body, contentType, "application/json")
	})
	if err != nil {
		writeFailure(w, err)
		return nil, false
	}
	defer sess.Close()
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAudioResponse))
	if err != nil {
		httpserver.WriteErrorCode(w, http.StatusBadGateway, "upstream_error", "the Console transcription could not be read")
		return nil, false
	}
	// Transcription is billed on the audio duration the upstream reports.
	var reported struct {
		Duration float64 `json:"duration"`
	}
	_ = json.Unmarshal(data, &reported)
	cost, priced := pricing.EstimateSTTCost(reported.Duration, false)
	h.recordMedia(r.Context(), sess.Account, row, cost, priced, map[string]any{"endpoint": "stt", "duration_seconds": reported.Duration})
	return data, true
}

// HandleSTT serves POST /stt with Console's own multipart fields.
func (h *Handler) HandleSTT(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	form, ok := readSTTForm(w, r, nil)
	if !ok {
		return
	}
	if data, ok := h.transcribe(w, r, form); ok {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
}

// HandleAudioTranscriptions serves the OpenAI-compatible
// POST /audio/transcriptions: response_format json (default), verbose_json
// (Console's full answer) or text.
func (h *Handler) HandleAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	format := ""
	form, ok := readSTTForm(w, r, map[string]bool{"response_format": true, "temperature": true, "prompt": true, "timestamp_granularities[]": true, "timestamp_granularities": true})
	if !ok {
		return
	}
	if r.MultipartForm != nil {
		format = strings.ToLower(strings.TrimSpace(util.FirstNonEmpty(r.MultipartForm.Value["response_format"]...)))
	}
	switch format {
	case "", "json", "verbose_json", "text":
	default:
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "response_format must be json, verbose_json or text")
		return
	}
	data, ok := h.transcribe(w, r, form)
	if !ok {
		return
	}
	var result struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(data, &result)
	switch format {
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, result.Text)
	case "verbose_json":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	default:
		util.WriteJSON(w, map[string]any{"text": result.Text})
	}
}

var realtimeUpgrader = websocket.Upgrader{
	// Inference authentication is the API key header, not a browser cookie,
	// so a cross-origin page gains nothing from the upgrade.
	CheckOrigin:     func(*http.Request) bool { return true },
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
}

// HandleRealtime relays GET /realtime (a WebSocket) to Console's realtime
// voice socket. The model comes from the query, like the OpenAI client sends it.
func (h *Handler) HandleRealtime(w http.ResponseWriter, r *http.Request) {
	model := qualify(r.URL.Query().Get("model"), defaultVoiceModel)
	row, ok := h.resolveForRequest(w, r, model, store.CapabilityRealtime)
	if !ok {
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "realtime takes a WebSocket upgrade")
		return
	}
	upstream, sess, err := h.dialRealtime(r.Context(), row)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer sess.Close()
	defer upstream.Close()
	client, err := realtimeUpgrader.Upgrade(hijackWriter{w}, r, nil)
	if err != nil {
		return
	}
	defer client.Close()
	done := make(chan struct{}, 2)
	pump := func(from, to *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			kind, message, err := from.ReadMessage()
			if err != nil {
				_ = to.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				return
			}
			if err := to.WriteMessage(kind, message); err != nil {
				return
			}
		}
	}
	go pump(client, upstream)
	go pump(upstream, client)
	<-done
}

// hijackWriter lets the WebSocket upgrader reach the connection through the
// middleware's response writer wrappers, which expose Unwrap rather than
// Hijack themselves.
type hijackWriter struct{ http.ResponseWriter }

func (w hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (h *Handler) dialRealtime(ctx context.Context, row *store.Model) (*websocket.Conn, *Session, error) {
	client := h.consoleClient()
	target, err := client.WebSocketURL("/realtime", url.Values{"model": {row.UpstreamModel}})
	if err != nil {
		return nil, nil, err
	}
	h.mu.RLock()
	transport := h.cfg.GrokWebTransport()
	h.mu.RUnlock()
	proxyFunc := httpclient.ProxyFuncFromConfig(transport)
	dialer := websocket.Dialer{Proxy: proxyFunc, HandshakeTimeout: 30 * time.Second}
	if strings.HasPrefix(target, "wss://") {
		// The handshake presents the same Chrome TLS fingerprint as the rest of
		// the grok.com traffic. The TLS dial goes through the proxy itself, so
		// the dialer must not wrap it in the proxy a second time.
		userAgent := client.userAgent()
		dialer.Proxy = nil
		dialer.NetDialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return httpclient.DialBrowserWebSocketTLS(ctx, network, addr, proxyFunc, userAgent)
		}
	}
	var conn *websocket.Conn
	_, sess, err := h.Call(ctx, row, func(sess *Session) (*http.Response, error) {
		for attempt := 0; attempt < 2; attempt++ {
			header, invalidate, err := client.WebSocketHeaders(ctx, sess.Token, target)
			if err != nil {
				return nil, err
			}
			c, resp, err := dialer.DialContext(ctx, target, header)
			if err == nil {
				conn = c
				return &http.Response{StatusCode: http.StatusSwitchingProtocols, Body: http.NoBody}, nil
			}
			if resp == nil {
				return nil, err
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
				invalidate()
				continue
			}
			if resp.StatusCode == http.StatusUnauthorized {
				return nil, ErrUnauthorized
			}
			return nil, &StatusError{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
		}
		return nil, errors.New("console.x.ai refused the realtime session")
	})
	if err != nil {
		return nil, nil, err
	}
	return conn, sess, nil
}
