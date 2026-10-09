package grok

import (
	"io"
	"net/http"

	"orchids-api/internal/httpserver"
)

func (h *Handler) withChatStream(req *http.Request, consume func(int, http.Header, io.Reader)) {
	httpserver.StreamThroughChat(req, h.HandleChatCompletions, consume)
}
