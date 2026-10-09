package util

import (
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// toolCallSequence is the process-wide counter behind NewToolCallID. It is
// shared so two channels minting an id in the same nanosecond cannot collide.
var toolCallSequence atomic.Uint64

// NewToolCallID mints a local tool-call id for an upstream delta that omits one.
//
// Every provider stream reader needs exactly this, and the ids only have to be
// unique within one stream, so one sequence serves them all.
func NewToolCallID() string {
	return fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), toolCallSequence.Add(1))
}

// NewThinkingSignature mints the per-stream thinking signature the Anthropic
// surface attaches to a thinking block. The prefix names the channel so a
// signature can be attributed when it round-trips in history replay.
func NewThinkingSignature(prefix string) string {
	var raw [24]byte
	if _, err := cryptorand.Read(raw[:]); err == nil {
		return prefix + ":" + base64.RawURLEncoding.EncodeToString(raw[:])
	}
	return fmt.Sprintf("%s:%d", prefix, time.Now().UnixNano())
}

// ToolCall accumulates one streamed tool call. Arguments arrive across several
// deltas, so they are buffered and read only once the call is emitted.
type ToolCall struct {
	ID   string
	Name string
	// Arguments holds every argument fragment seen for this call, in arrival
	// order. It is exported so a stream reader can hand the buffer to
	// the emitter without copying; only the accumulator writes to it.
	Arguments string
	// emitted marks a call that has already been delivered. Draining is what
	// makes a double flush safe: the finish-time emit and the end-of-stream
	// flush both run on the same accumulator.
	emitted bool
}

// ToolCallAccumulator rebuilds tool calls from OpenAI-style deltas, where the
// name arrives in the first delta and the arguments are streamed afterwards.
//
// The upstream sometimes reuses one index for calls that are only distinguished
// by id. The order therefore stores call instances rather than indexes; the map
// only identifies which instance receives an id-less continuation delta.
type ToolCallAccumulator struct {
	order []*ToolCall
	calls map[int]*ToolCall
}

func NewToolCallAccumulator() *ToolCallAccumulator {
	return &ToolCallAccumulator{calls: map[int]*ToolCall{}}
}

// Add folds one delta into the call at index.
func (a *ToolCallAccumulator) Add(index int, id, name, args string) *ToolCall {
	trimmedID := strings.TrimSpace(id)
	state, ok := a.calls[index]
	if ok && (state.emitted || (trimmedID != "" && state.ID != "" && state.ID != trimmedID)) {
		// A closed call or a new id at the same index starts another call
		// instance. Keeping the old pointer in order preserves parallel calls.
		state = nil
		ok = false
	}
	if !ok {
		state = &ToolCall{}
		a.calls[index] = state
		a.order = append(a.order, state)
	}
	if trimmedID != "" {
		state.ID = trimmedID
	}
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		state.Name = trimmed
	}
	state.Arguments += args
	return state
}

// CompleteAll drains every not-yet-emitted call in stream order and marks them
// delivered, so a second flush cannot hand the same call to the client twice.
func (a *ToolCallAccumulator) CompleteAll() []*ToolCall {
	out := make([]*ToolCall, 0, len(a.order))
	for _, state := range a.order {
		if state == nil || state.emitted || state.Name == "" {
			continue
		}
		state.emitted = true
		out = append(out, state)
	}
	return out
}

// UsageInt reads the first present numeric value among keys. Upstreams disagree
// on both the key name and whether the number arrives as float64, int or
// json.Number, so every shape is accepted.
func UsageInt(values map[string]interface{}, keys ...string) (int, bool) {
	for _, key := range keys {
		switch typed := values[key].(type) {
		case float64:
			return int(typed), true
		case int:
			return typed, true
		case json.Number:
			if parsed, err := typed.Int64(); err == nil {
				return int(parsed), true
			}
		}
	}
	return 0, false
}

// UsageMap reads the first present nested object among keys.
func UsageMap(values map[string]interface{}, keys ...string) map[string]interface{} {
	for _, key := range keys {
		if nested, ok := values[key].(map[string]interface{}); ok {
			return nested
		}
	}
	return nil
}
