package grok

import (
	"fmt"
	"io"
	"net/http"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"strings"
	"time"
	"unicode/utf8"

	"orchids-api/internal/audit"
)

type chatOutcome struct {
	Usage       map[string]any
	UsageSource audit.UsageSource
	// ServedModel is the model the upstream named on its response object.
	ServedModel string
	Finish      string
	FirstToken  time.Time
	Err         error
	SawText     bool
}

// stopFilter keeps only a suffix that could still become a stop sequence.
// It operates on the original deltas: trimming whitespace corrupts tool JSON
// and text, and a stop sequence can span multiple upstream events.
type stopFilter struct {
	sequences []string
	pending   string
	matched   string
}

func (f *stopFilter) push(delta string, flush bool) string {
	if f.matched != "" {
		return ""
	}
	f.pending += delta
	cut, match := len(f.pending), ""
	for _, stop := range f.sequences {
		if stop == "" {
			continue
		}
		if i := strings.Index(f.pending, stop); i >= 0 && (match == "" || i < cut) {
			cut, match = i, stop
		}
	}
	if match != "" {
		out := f.pending[:cut]
		f.pending = ""
		f.matched = match
		return out
	}
	keep := 0
	if !flush {
		for _, stop := range f.sequences {
			for n := 1; n < len(stop) && n <= len(f.pending); n++ {
				if strings.HasSuffix(f.pending, stop[:n]) && n > keep {
					keep = n
				}
			}
		}
	}
	cut = len(f.pending) - keep
	for cut > 0 && !utf8.ValidString(f.pending[:cut]) {
		cut--
	}
	out := f.pending[:cut]
	f.pending = f.pending[cut:]
	return out
}

type responseToolState struct {
	itemID, callID, name string
	arguments            strings.Builder
	index                int
}
type responseReasoningState struct {
	source, signature, key string
	text                   strings.Builder
}

// streamBuildChat relays a Build Responses stream as Chat Completions chunks.
//
// The per-stream state and the handling of each Responses event live on
// buildChatStream (native_chat_stream_state.go); this function is the order:
// open, relay every upstream event, then finish or fail.
func (h *Handler) streamBuildChat(w http.ResponseWriter, req *chatwire.Request, body io.Reader) (outcome chatOutcome) {
	s := newBuildChatStream(h, w, req, &outcome)
	if err := s.emit(map[string]any{"role": "assistant"}, "", nil); err != nil {
		outcome.Err = err
		return
	}
	err := responses.ReadSSE(body, s.onEvent)
	if err != nil && err != io.EOF {
		s.fail(fmt.Errorf("build stream read error: %w", err))
		return
	}
	if !s.terminal {
		s.fail(fmt.Errorf("upstream stream closed before response completion"))
		return
	}
	s.finish()
	return
}
