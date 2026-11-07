package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"orchids-api/internal/testutil"
)

// fakeLiveKit answers /rtc with an echoing WebSocket (refusing a bad token)
// and /rtc/validate with a plain reason, recording the query it received.
func fakeLiveKit(t *testing.T) (*httptest.Server, *url.Values) {
	t.Helper()
	var seen url.Values
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query()
		if r.URL.Query().Get("access_token") == "expired" {
			http.Error(w, "token expired", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/rtc":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				kind, data, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if err := conn.WriteMessage(kind, append([]byte("echo:"), data...)); err != nil {
					return
				}
			}
		case "/rtc/validate":
			_, _ = w.Write([]byte("success"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	oldHost, oldSchemes := voiceRelayHost, voiceRelaySchemes
	voiceRelayHost, voiceRelaySchemes = host, map[string]string{"ws": "ws", "http": "http"}
	t.Cleanup(func() { voiceRelayHost, voiceRelaySchemes = oldHost, oldSchemes })
	return server, &seen
}

func relayServer(t *testing.T) *httptest.Server {
	t.Helper()
	h := &Handler{}
	server := httptest.NewServer(http.HandlerFunc(h.HandleAdminVoiceRelay))
	t.Cleanup(server.Close)
	return server
}

func dialRelay(server *httptest.Server, origin, token string) (*websocket.Conn, *http.Response, error) {
	target := "ws" + strings.TrimPrefix(server.URL, "http") + VoiceRelayPath + "/rtc?access_token=" + token + "&protocol=15"
	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	return websocket.DefaultDialer.Dial(target, header)
}

// The relay carries signal frames both ways and passes the SDK's query
// (the room token among it) to the upstream unchanged.
func TestVoiceRelayCarriesSignalFrames(t *testing.T) {
	_, seen := fakeLiveKit(t)
	relay := relayServer(t)
	conn, _, err := dialRelay(relay, relay.URL, "room-token")
	testutil.NoError(t, err, "dial relay")
	defer conn.Close()
	testutil.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3}), "write")
	kind, data, err := conn.ReadMessage()
	testutil.NoError(t, err, "read")
	testutil.Equal(t, kind, websocket.BinaryMessage)
	testutil.Equal(t, string(data), "echo:\x01\x02\x03")
	testutil.Equal(t, seen.Get("access_token"), "room-token")
	testutil.Equal(t, seen.Get("protocol"), "15")
}

// A handshake from another site must not ride the admin session cookie.
func TestVoiceRelayRejectsCrossOrigin(t *testing.T) {
	fakeLiveKit(t)
	relay := relayServer(t)
	for _, origin := range []string{"", "https://evil.example", "null"} {
		_, resp, err := dialRelay(relay, origin, "room-token")
		testutil.Error(t, err, "origin "+origin)
		testutil.Equal(t, resp.StatusCode, http.StatusForbidden)
	}
}

// An upstream refusal keeps its status, so the SDK reports it, and the
// validate request it then makes reaches the upstream too.
func TestVoiceRelayPassesUpstreamRefusalAndValidate(t *testing.T) {
	fakeLiveKit(t)
	relay := relayServer(t)
	_, resp, err := dialRelay(relay, relay.URL, "expired")
	testutil.Error(t, err, "expired token")
	testutil.Equal(t, resp.StatusCode, http.StatusUnauthorized)

	validate, err := http.Get(relay.URL + VoiceRelayPath + "/rtc/validate?access_token=room-token")
	testutil.NoError(t, err, "validate")
	defer validate.Body.Close()
	testutil.Equal(t, validate.StatusCode, http.StatusOK)

	other, err := http.Get(relay.URL + VoiceRelayPath + "/anything")
	testutil.NoError(t, err, "other path")
	other.Body.Close()
	testutil.Equal(t, other.StatusCode, http.StatusNotFound)
}
