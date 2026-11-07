package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"orchids-api/internal/httpclient"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// appChatTransport separates the legacy REST app-chat wire format from MGW.
// Keeping this at the client boundary prevents an SSO cookie from ever being
// used as a Build/CLI Bearer token.
type appChatTransport interface {
	Chat(context.Context, *Client, string, map[string]any) (*http.Response, error)
}

type restAppChatTransport struct{}

func (restAppChatTransport) Chat(ctx context.Context, client *Client, token string, payload map[string]any) (*http.Response, error) {
	return client.doRESTChat(ctx, token, payload)
}

var errMGWRequiresExplicitRequest = errors.New("mgw websocket requires explicit user id, model, and prompt")

// appChatFallbackTransport considers MGW first only when a caller supplied its
// explicit request contract.  The existing REST call sites supply a different
// payload and therefore always take the safe, protocol-preserving fallback.
type appChatFallbackTransport struct {
	mgw  *mgwWebSocketTransport
	rest appChatTransport
}

func newAppChatFallbackTransport(client *Client) *appChatFallbackTransport {
	return &appChatFallbackTransport{mgw: &mgwWebSocketTransport{client: client}, rest: restAppChatTransport{}}
}

func (t *appChatFallbackTransport) Chat(ctx context.Context, client *Client, token string, payload map[string]any) (*http.Response, error) {
	if t != nil && t.mgw != nil {
		_, _ = t.mgw.fromLegacyPayload(token, payload)
	}
	if t == nil || t.rest == nil {
		return restAppChatTransport{}.Chat(ctx, client, token, payload)
	}
	return t.rest.Chat(ctx, client, token, payload)
}

const (
	mgwPath             = "/ws/mgw/"
	mgwHandshakeTimeout = 20 * time.Second
	mgwMaxFrameBytes    = 16 << 20
)

// MGWChatRequest is intentionally distinct from the REST app-chat payload.
// UserID is passed as a websocket query parameter, while the SSO credential is
// sent only in the Cookie header.
type MGWChatRequest struct {
	UserID string
	Model  string
	Prompt string
	Token  string
}

type mgwWebSocketTransport struct {
	client *Client
}

func (t *mgwWebSocketTransport) fromLegacyPayload(_ string, _ map[string]any) (MGWChatRequest, error) {
	// The old payload has no stable user id and can contain app-chat-only model
	// controls/attachments.  Guessing those fields could send an altered request
	// to an account, so no implicit conversion is permitted.
	return MGWChatRequest{}, errMGWRequiresExplicitRequest
}

// Open opens an MGW stream for callers that explicitly provide the validated
// MGW contract. It is not wired into legacy REST handlers until their response
// parser can consume MGW events. Egress is deliberately fail-closed because the
// current egress manager exposes HTTP leases only, not websocket leases.
func (t *mgwWebSocketTransport) Open(ctx context.Context, request MGWChatRequest) (*http.Response, error) {
	if t == nil || t.client == nil {
		return nil, fmt.Errorf("grok mgw transport not configured")
	}
	if t.client.egress != nil && t.client.egress.Routes("app_chat") {
		return nil, fmt.Errorf("grok mgw websocket unavailable: egress websocket lease is not supported")
	}
	if strings.TrimSpace(request.UserID) == "" || strings.TrimSpace(request.Model) == "" || strings.TrimSpace(request.Prompt) == "" || strings.TrimSpace(request.Token) == "" {
		return nil, errMGWRequiresExplicitRequest
	}
	endpoint, origin, err := mgwEndpoint(t.client.baseURL(), request.UserID)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: mgwHandshakeTimeout, Proxy: mgwProxyFunc(t.client)}
	connection, handshake, err := dialer.DialContext(ctx, endpoint, mgwHeaders(t.client, origin, request.UserID, request.Token))
	if err != nil {
		if handshake != nil {
			return handshake, nil
		}
		return nil, fmt.Errorf("grok mgw websocket dial: %w", err)
	}
	reader, writer := io.Pipe()
	go streamMGW(ctx, connection, writer, request)
	httpRequest, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"application/x-ndjson"}}, Body: reader, Request: httpRequest}, nil
}

func mgwEndpoint(baseURL, userID string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return "", "", fmt.Errorf("grok mgw base url is invalid")
	}
	origin := (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", "", fmt.Errorf("grok mgw base url scheme is invalid")
	}
	parsed.Path, parsed.RawPath = mgwPath, ""
	parsed.RawQuery = url.Values{"uid": []string{strings.TrimSpace(userID)}}.Encode()
	parsed.Fragment = ""
	return parsed.String(), origin, nil
}

func mgwHeaders(client *Client, origin, userID, token string) http.Header {
	header := http.Header{}
	header.Set("Origin", origin)
	header.Set("User-Agent", client.userAgent())
	header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	header.Set("Cache-Control", "no-cache")
	header.Set("Pragma", "no-cache")
	// SSO is a browser cookie, never an Authorization: Bearer credential.
	if cookie := mgwCookie(token, userID); cookie != "" {
		header.Set("Cookie", cookie)
	}
	return header
}

func mgwCookie(token, userID string) string {
	items := grokCookieItems(buildGrokCookie(token, "", ""))
	withoutUserID := items[:0]
	for _, item := range items {
		if item.name != "x-userid" {
			withoutUserID = append(withoutUserID, item)
		}
	}
	setGrokCookieItem(&withoutUserID, "x-userid", userID)
	return joinGrokCookieItems(withoutUserID)
}

func mgwProxyFunc(client *Client) func(*http.Request) (*url.URL, error) {
	if client == nil {
		return nil
	}
	return httpclient.ProxyFuncFromConfig(client.cfg)
}

func streamMGW(ctx context.Context, connection *websocket.Conn, writer *io.PipeWriter, request MGWChatRequest) {
	defer connection.Close()
	defer writer.Close()
	connection.SetReadLimit(mgwMaxFrameBytes)
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetReadDeadline(deadline)
	}
	sender := &mgwSender{connection: connection}
	initialID := "evt_init_" + randomUUID()
	if err := sender.write(map[string]any{"event": map[string]any{
		"type": "session.create", "event_id": initialID,
		"session": map[string]any{"model": request.Model, "x_grok": map[string]any{"protocol_capabilities": []string{"conversation_attached", "custom_methods_v1"}, "use_chunk": true, "is_temporary": true, "disable_memory": true}},
	}}); err != nil {
		_ = writer.CloseWithError(fmt.Errorf("send grok mgw session.create: %w", err))
		return
	}
	created, attached, sent := false, false, false
	for {
		messageType, data, err := connection.ReadMessage()
		if err != nil {
			_ = writer.CloseWithError(fmt.Errorf("read grok mgw: %w", err))
			return
		}
		if messageType != websocket.TextMessage || len(data) > mgwMaxFrameBytes {
			continue
		}
		frame := make([]byte, len(data)+1)
		copy(frame, data)
		frame[len(data)] = '\n'
		if _, err := writer.Write(frame); err != nil {
			return
		}
		eventType, sessionID := mgwEventMetadata(data)
		switch eventType {
		case "session.created":
			created = true
		case "conversation.attached":
			attached = true
		case "response.done", "error":
			return
		case "session.ended":
			_ = writer.CloseWithError(fmt.Errorf("grok mgw session ended before response completion"))
			return
		}
		if created && attached && !sent && sessionID != "" {
			sent = true
			now := time.Now().UnixMilli()
			item := map[string]any{"session_id": sessionID, "event": map[string]any{"type": "conversation.item.create", "event_id": fmt.Sprintf("evt_msg_%d", now), "item": map[string]any{"type": "message", "role": "user", "x_grok": map[string]any{"client_message_id": randomUUID(), "input_chunks": []any{map[string]any{"text": map[string]any{"text": request.Prompt}}}}}}}
			response := map[string]any{"session_id": sessionID, "event": map[string]any{"type": "response.create", "event_id": fmt.Sprintf("evt_resp_%d", now)}}
			if err := sender.write(item); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			if err := sender.write(response); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
		}
	}
}

type mgwSender struct {
	mu         sync.Mutex
	connection *websocket.Conn
}

func (s *mgwSender) write(value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.connection.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return s.connection.WriteJSON(value)
}

func mgwEventMetadata(data []byte) (eventType, sessionID string) {
	var envelope struct {
		SessionID string `json:"session_id"`
		Event     struct {
			Type         string `json:"type"`
			Conversation struct {
				ID string `json:"id"`
			} `json:"conversation"`
		} `json:"event"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return "", ""
	}
	sessionID = strings.TrimSpace(envelope.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(envelope.Event.Conversation.ID)
	}
	return envelope.Event.Type, sessionID
}
