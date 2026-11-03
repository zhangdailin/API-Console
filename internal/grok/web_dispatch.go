package grok

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"orchids-api/internal/grok/web"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/responses"
)

// The grok channel serves two planes behind one provider prefix. Build models
// are handled by this package; Grok Web (grok.com SSO) models are handled by
// internal/grok/web. A request is dispatched on its model identifier, which is
// the only thing the two planes' route tables do not share.

// Web returns the Grok Web plane handler.
func (h *Handler) Web() *web.Handler {
	if h == nil {
		return nil
	}
	return h.web
}

// peekWebModel reads the request body, restores it for the next reader and
// reports whether its model names a Grok Web route. A body that cannot be read
// is answered here and reported as handled.
func peekWebModel(w http.ResponseWriter, r *http.Request) (isWeb, handled bool) {
	if r == nil || r.Body == nil || r.Method != http.MethodPost {
		return false, false
	}
	body, err := httpserver.ReadBoundedJSONBody(w, r)
	if err != nil {
		return false, true
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return false, false
	}
	return web.IsWebModelID(probe.Model), false
}

// webResponsesBridge serves /responses for Web models through the shared
// chat→Responses bridge, over the Web plane's chat completions handler.
func (h *Handler) webResponsesBridge() http.HandlerFunc {
	h.webBridgeOnce.Do(func() {
		options := responses.BridgeOptions{}
		if h.lb != nil && h.lb.Store != nil {
			options.Store = h.lb.Store
		}
		if cfg := h.configSnapshot(); cfg != nil && cfg.ResponseStoreTTL > 0 {
			options.TTL = time.Duration(cfg.ResponseStoreTTL) * time.Hour
		}
		h.webBridge = responses.ResponsesBridgeHandler(h.web.HandleChatCompletions, options)
	})
	return h.webBridge
}

type webDispatch struct {
	webBridgeOnce sync.Once
	webBridge     http.HandlerFunc
}
