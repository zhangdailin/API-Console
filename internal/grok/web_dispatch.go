package grok

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"orchids-api/internal/grok/web"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/responses"
)

// Web returns the Grok Web plane handler.
func (h *Handler) Web() *web.Handler {
	if h == nil {
		return nil
	}
	return h.web
}

// peekWebModel reads and restores the JSON body to identify Web models.
// Read failures have already been answered when handled is true.
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
