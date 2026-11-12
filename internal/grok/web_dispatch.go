package grok

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/grok/console"
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

// Console returns the Grok Console plane handler.
func (h *Handler) Console() *console.Handler {
	if h == nil {
		return nil
	}
	return h.console
}

// plane names the route plane a request's model belongs to.
type plane int

const (
	planeBuild plane = iota
	planeWeb
	planeConsole
)

// peekPlane reads a JSON request body, restores it for the next reader and
// reports which plane its model names. A body that cannot be read is answered
// here and reported as handled.
func peekPlane(w http.ResponseWriter, r *http.Request) (plane, bool) {
	if r == nil || r.Body == nil || r.Method != http.MethodPost {
		return planeBuild, false
	}
	body, err := httpserver.ReadBoundedJSONBody(w, r)
	if err != nil {
		return planeBuild, true
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return planeBuild, false
	}
	switch {
	case console.IsConsoleModelID(probe.Model):
		return planeConsole, false
	case web.IsWebModelID(probe.Model):
		return planeWeb, false
	}
	return planeBuild, false
}

// peekWebModel reports whether a request's model names a Grok Web route.
func peekWebModel(w http.ResponseWriter, r *http.Request) (isWeb, handled bool) {
	p, handled := peekPlane(w, r)
	return p == planeWeb, handled
}

// HandleImagesGenerations dispatches image generation between the Web and
// Console planes.
func (h *Handler) HandleImagesGenerations(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/") {
		if p, handled := peekPlane(w, r); handled {
			return
		} else if p == planeConsole && h.console != nil {
			h.console.HandleImagesGenerations(w, r)
			return
		}
	}
	h.web.HandleImagesGenerations(w, r)
}

// HandleImagesEdits dispatches image edits. A multipart upload is parsed
// once here to read its model; parsing again in either plane is a no-op.
func (h *Handler) HandleImagesEdits(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/") {
		if r.Method == http.MethodPost && h.console != nil {
			r.Body = http.MaxBytesReader(w, r.Body, console.MaxImageEditUpload)
			if err := r.ParseMultipartForm(console.MaxImageEditUpload); err != nil {
				httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "the multipart body could not be read")
				return
			}
			if console.IsConsoleModelID(r.FormValue("model")) {
				h.console.HandleImagesEdits(w, r)
				return
			}
		}
	} else if p, handled := peekPlane(w, r); handled {
		return
	} else if p == planeConsole && h.console != nil {
		h.console.HandleImagesEdits(w, r)
		return
	}
	h.web.HandleImagesEdits(w, r)
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
