package warp

import (
	"crypto/sha256"
	"encoding/hex"
)

// Wire identifiers are untrusted, even in model metadata. Only identities
// already advertised by the account may appear verbatim in diagnostics.
type observedModelMismatch struct {
	ID                     string
	Fallback               bool
	Event, Action, Message int
}

type modelMismatchDiagnostic struct {
	RequestedModel      string `json:"requested_model"`
	ReportedModel       string `json:"reported_model,omitempty"`
	ReportedModelSHA256 string `json:"reported_model_sha256"`
	ReportedModelBytes  int    `json:"reported_model_bytes"`
	ReportedModelKnown  bool   `json:"reported_model_known"`
	Fallback            bool   `json:"fallback"`
	Reason              string `json:"reason"`
	EventOrdinal        int    `json:"event_ordinal"`
	ActionOrdinal       int    `json:"action_ordinal"`
	MessageOrdinal      int    `json:"message_ordinal"`
}

func modelMismatchDetails(requested string, observed *observedModelMismatch, known []string) *modelMismatchDiagnostic {
	if observed == nil {
		return nil
	}
	digest := sha256.Sum256([]byte(observed.ID))
	d := &modelMismatchDiagnostic{RequestedModel: requested, ReportedModelSHA256: hex.EncodeToString(digest[:]), ReportedModelBytes: len(observed.ID), Fallback: observed.Fallback, Reason: "model_id_mismatch", EventOrdinal: observed.Event, ActionOrdinal: observed.Action, MessageOrdinal: observed.Message}
	if observed.ID == requested {
		d.Reason = "matched"
	}
	if observed.Fallback {
		d.Reason = "fallback"
		if observed.ID != requested {
			d.Reason = "fallback_and_model_id_mismatch"
		}
	}
	for _, id := range append([]string{requested}, known...) {
		if id == observed.ID && len(id) <= 256 && validModel(id) {
			d.ReportedModel, d.ReportedModelKnown = id, true
			break
		}
	}
	return d
}
