package responses

import (
	"io"
	"strings"
)

// MaxEventBytes bounds one SSE frame. A scanner with no bound lets a single
// upstream frame allocate arbitrary memory in the gateway.
const MaxEventBytes = 8 << 20

// ReadSSEBytes consumes whole SSE frames, including multi-line data, while
// keeping payloads as bytes. Callers that decode JSON can therefore pass the
// payload straight to json.Unmarshal without a string -> []byte round trip.
func ReadSSEBytes(reader io.Reader, consume func(string, []byte) error) error {
	return ConsumeSSE(reader, func(event SSEEvent) error {
		if !event.HasData() {
			return nil
		}
		return consume(event.Event, event.Data())
	})
}

// ReadSSE retains the string callback used by text-oriented callers.
func ReadSSE(reader io.Reader, consume func(string, string) error) error {
	return ReadSSEBytes(reader, func(event string, data []byte) error {
		return consume(event, string(data))
	})
}

// IsPrivateBuildControlEvent reports whether an event is Grok Build's private
// control traffic rather than part of the public Responses stream. It matches
// both the SSE event name and the payload `type`, because the upstream emits
// the marker in either place, so both spellings are matched.
func IsPrivateBuildControlEvent(kind string) bool {
	return strings.TrimSpace(kind) == "response.doom_loop_check"
}
