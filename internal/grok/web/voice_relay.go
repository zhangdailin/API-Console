package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"orchids-api/internal/httpclient"
)

// The voice relay carries the LiveKit signal connection through this server.
//
// The browser normally opens wss://livekit.grok.com itself. Where that host is
// unreachable from the browser's network (mainland China, for example), the
// tools page connects to this path instead and the server opens the upstream
// connection. Only the signal channel is relayed: the room token travels in
// the query exactly as the LiveKit SDK sends it, and the media (WebRTC) still
// goes to the ICE candidates LiveKit hands out.
//
// The upstream is fixed. Nothing in the request chooses where the server
// connects, so the relay cannot be pointed at another host.
const (
	VoiceRelayPath     = "/api/grok/admin/voice/livekit"
	voiceRelayUpstream = "livekit.grok.com"

	voiceRelayHandshakeTimeout = 15 * time.Second
	voiceRelayReadLimit        = 4 << 20
	voiceRelayValidateLimit    = 64 << 10
)

// HandleAdminVoiceRelay serves the two paths the LiveKit SDK derives from the
// URL it is given: <url>/rtc (the signal WebSocket) and <url>/rtc/validate
// (a GET it issues to explain a failed connection).
func (h *Handler) HandleAdminVoiceRelay(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimPrefix(r.URL.Path, VoiceRelayPath) {
	case "/rtc":
		h.relayVoiceSignal(w, r)
	case "/rtc/validate":
		h.relayVoiceValidate(w, r)
	default:
		http.NotFound(w, r)
	}
}

// sameOriginUpgrade accepts a handshake only from this admin page. The route
// is cookie-authenticated and a WebSocket handshake is a GET, which the admin
// write guard does not inspect, so the origin is checked here.
func sameOriginUpgrade(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin == nil || (origin.Scheme != "http" && origin.Scheme != "https") {
		return false
	}
	if origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return false
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	return strings.EqualFold(origin.Host, r.Host)
}

func (h *Handler) voiceRelayProxy() func(*http.Request) (*url.URL, error) {
	if client := h.webClient(); client != nil {
		return httpclient.ProxyFuncFromConfig(client.cfg)
	}
	return http.ProxyFromEnvironment
}

// voiceRelaySchemes maps the relayed request kind to the upstream scheme;
// tests replace it (and voiceRelayHost) to point at a local server.
var (
	voiceRelayHost    = voiceRelayUpstream
	voiceRelaySchemes = map[string]string{"ws": "wss", "http": "https"}
)

func voiceRelayUpstreamURL(kind, path string, query url.Values) string {
	u := url.URL{Scheme: voiceRelaySchemes[kind], Host: voiceRelayHost, Path: path, RawQuery: query.Encode()}
	return u.String()
}

func (h *Handler) relayVoiceSignal(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return
	}
	if !sameOriginUpgrade(r) {
		http.Error(w, "voice relay requires the same origin", http.StatusForbidden)
		return
	}

	// Dial first: an upstream refusal (an expired token is a 401) is answered
	// as an HTTP status, which is what the SDK reports and then validates.
	dialer := websocket.Dialer{HandshakeTimeout: voiceRelayHandshakeTimeout, Proxy: h.voiceRelayProxy()}
	header := http.Header{}
	if ua := r.Header.Get("User-Agent"); ua != "" {
		header.Set("User-Agent", ua)
	}
	ctx, cancel := context.WithTimeout(r.Context(), voiceRelayHandshakeTimeout)
	upstream, resp, err := dialer.DialContext(ctx, voiceRelayUpstreamURL("ws", "/rtc", r.URL.Query()), header)
	cancel()
	if err != nil {
		status := http.StatusBadGateway
		if resp != nil && resp.StatusCode >= 400 && resp.StatusCode < 500 {
			status = resp.StatusCode
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		slog.Warn("voice relay: upstream signal connection failed", "status", status, "error", err)
		http.Error(w, "could not reach the LiveKit signal server", status)
		return
	}
	defer upstream.Close()

	upgrader := websocket.Upgrader{CheckOrigin: sameOriginUpgrade}
	client, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has already answered the request.
	}
	defer client.Close()
	// The HTTP server's request deadlines stay on a hijacked connection; a
	// signal session outlives them.
	_ = client.NetConn().SetDeadline(time.Time{})
	client.SetReadLimit(voiceRelayReadLimit)
	upstream.SetReadLimit(voiceRelayReadLimit)

	pipeVoiceSignal(client, upstream)
}

// pipeVoiceSignal copies frames both ways until either side closes, then
// passes the close on to the other side.
func pipeVoiceSignal(client, upstream *websocket.Conn) {
	var once sync.Once
	done := make(chan struct{})
	finish := func(to *websocket.Conn, err error) {
		once.Do(func() {
			code, text := websocket.CloseNormalClosure, ""
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				code, text = closeErr.Code, closeErr.Text
			}
			if code == websocket.CloseNoStatusReceived || code == websocket.CloseAbnormalClosure {
				code = websocket.CloseGoingAway
			}
			_ = to.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(time.Second))
			close(done)
		})
	}
	copyFrames := func(dst, src *websocket.Conn) {
		for {
			kind, reader, err := src.NextReader()
			if err != nil {
				finish(dst, err)
				return
			}
			writer, err := dst.NextWriter(kind)
			if err != nil {
				finish(src, err)
				return
			}
			if _, err := io.Copy(writer, reader); err != nil {
				finish(src, err)
				return
			}
			if err := writer.Close(); err != nil {
				finish(src, err)
				return
			}
		}
	}
	go copyFrames(upstream, client)
	go copyFrames(client, upstream)
	<-done
}

// relayVoiceValidate forwards the SDK's explanation request, so a failed
// relay connection reports the upstream's reason (for example an expired
// token) instead of a generic network error.
func (h *Handler) relayVoiceValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), voiceRelayHandshakeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, voiceRelayUpstreamURL("http", "/rtc/validate", r.URL.Query()), nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// A rare, one-off request: no pooled connections to leave behind.
	client := &http.Client{Transport: &http.Transport{Proxy: h.voiceRelayProxy(), DisableKeepAlives: true}, Timeout: voiceRelayHandshakeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "could not reach the LiveKit signal server", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, voiceRelayValidateLimit))
}
