package handler

import (
	"strings"

	"orchids-api/internal/perf"
)

// setStructuredOutput installs the validator a strict structured-output request
// carries. Nil is the ordinary case: a request that asked for no schema, or a
// non-strict one, has nothing to enforce.
func (h *streamHandler) setStructuredOutput(check structuredOutputCheck) {
	h.mu.Lock()
	h.structured = check
	h.mu.Unlock()
}

// structuredOutputCheck validates one completed assistant answer against the
// schema the caller declared. It is a function rather than an interface so the
// handler keeps no reference to the request's schema object.
type structuredOutputCheck func(text string) error

// validateStructuredAnswer checks the answer accumulated so far against the
// schema of a strict structured-output request. It returns nil when the request
// declared no schema, and when the answer matches.
//
// Only the visible text is checked: reasoning is not part of the answer the
// caller parses, and tool calls are validated by the tool pipeline already. An
// empty answer is left to the empty-output fallback below rather than rejected
// here, so a request that produced nothing still reports that.
func (h *streamHandler) validateStructuredAnswer() error {
	h.mu.Lock()
	check := h.structured
	text := h.responseText.String()
	h.mu.Unlock()
	if check == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	return check(text)
}

// growBuilderSlots extends a builder slice so idx is addressable. Content-block
// indices are dense and monotonic, so this grows by one slot per block in
// practice and keeps append's amortised doubling for the rest.
func growBuilderSlots(slots []*strings.Builder, idx int) []*strings.Builder {
	for len(slots) <= idx {
		slots = append(slots, nil)
	}
	return slots
}

// growSigSlots extends the signature slice so idx is addressable. The slice is
// grown only by the thinking-block creation path, so its length is exactly one
// past the highest index that path ever wrote — which is what makes the bounds
// check in thinkingBlockSigAt reproduce the map's "key present" test.
func growSigSlots(sigs []string, idx int) []string {
	for len(sigs) <= idx {
		sigs = append(sigs, "")
	}
	return sigs
}

// thinkingBlockSigAt returns the stored signature for a thinking-block index. The
// boolean reproduces the map's "key present" result, so an index past the end
// reads as absent rather than as present-and-empty — the difference that keeps
// the callers below from writing outside the slice.
func (h *streamHandler) thinkingBlockSigAt(idx int) (string, bool) {
	if idx < 0 || idx >= len(h.thinkingBlockSigs) {
		return "", false
	}
	return h.thinkingBlockSigs[idx], true
}

// builderAt returns the history builder already recorded at idx, or nil. It
// never allocates, for readers that must not grow the slice.
func builderAt(slots []*strings.Builder, idx int) *strings.Builder {
	if idx < 0 || idx >= len(slots) {
		return nil
	}
	return slots[idx]
}

// historyBuilderAt returns the history builder for a content-block index,
// allocating the slot and the builder on first use. The caller must hold h.mu.
func (h *streamHandler) historyBuilderAt(slots *[]*strings.Builder, idx int) *strings.Builder {
	if idx < 0 {
		return nil
	}
	*slots = growBuilderSlots(*slots, idx)
	if (*slots)[idx] == nil {
		(*slots)[idx] = perf.AcquireStringBuilder()
	}
	return (*slots)[idx]
}

func (h *streamHandler) release() {
	perf.ReleaseStringBuilder(h.responseText)
	for _, sb := range h.textBlockBuilders {
		perf.ReleaseStringBuilder(sb)
	}
	for _, sb := range h.thinkingBlockBuilders {
		perf.ReleaseStringBuilder(sb)
	}
}
