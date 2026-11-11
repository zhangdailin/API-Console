package cline

import (
	"io"
	"orchids-api/internal/upstream"
)

// buildChatBody renders one chat request.
func buildChatBody(req upstream.UpstreamRequest, model string) ([]byte, error) {
	payload, err := buildChatPayload(req, model)
	return payload.body, err
}

// consumeStream parses the SSE body and forwards deltas to the caller.
func consumeStream(body io.Reader, toolsEnabled bool, onMessage func(upstream.SSEMessage), streamToolDeltas ...bool) (streamResult, error) {
	return consumeStreamWithToolNames(body, toolsEnabled, nil, onMessage, len(streamToolDeltas) > 0 && streamToolDeltas[0])
}
