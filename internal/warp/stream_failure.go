package warp

import (
	"errors"
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
)

// streamFailure contains only fixed reason identifiers, enum ordinals and byte
// counts. It never includes masks, identifiers or provider payloads.
type streamFailure struct {
	Site          string `json:"site"`
	Reason        string `json:"reason"`
	EventOrdinal  int    `json:"event_ordinal,omitempty"`
	ActionOrdinal int    `json:"action_ordinal,omitempty"`
	ActionKind    int    `json:"action_kind,omitempty"`
	ReasonKind    int    `json:"reason_kind,omitempty"`
	MeasuredBytes int    `json:"measured_bytes,omitempty"`
	LimitBytes    int    `json:"limit_bytes,omitempty"`
}

func failureFromState(site string, state *continuationAccumulator) *streamFailure {
	out := &streamFailure{Site: site, Reason: "invalid_state", LimitBytes: continuationStateLimit}
	if state == nil {
		return out
	}
	if state.root != nil {
		out.MeasuredBytes = proto.Size(warpapi.Request_TaskContext_builder{Tasks: []*warpapi.Task{state.root}}.Build())
	}
	var detail *continuationStateError
	if errors.As(state.err, &detail) {
		out.Reason = detail.Reason
		out.ActionKind = detail.ActionKind
		if detail.MeasuredBytes > 0 {
			out.MeasuredBytes = detail.MeasuredBytes
		}
		if detail.LimitBytes > 0 {
			out.LimitBytes = detail.LimitBytes
		}
	}
	return out
}
