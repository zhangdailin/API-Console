package warp

import (
	"context"
	"maps"
	"strings"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"

	"orchids-api/internal/upstream"
)

// ModelUsed belongs to the current exchange, not the requested alias or an
// inherited task message. Retain effective updates and transaction rollback.
type modelObservations struct {
	inherited   map[string]bool
	current     map[string]observedModelMismatch
	transaction map[string]observedModelMismatch
}

func newModelObservations(root *warpapi.Task) *modelObservations {
	o := &modelObservations{inherited: map[string]bool{}, current: map[string]observedModelMismatch{}}
	for _, message := range root.GetMessages() {
		if message.GetModelUsed() != nil {
			o.inherited[message.GetId()] = true
		}
	}
	return o
}

func (o *modelObservations) begin()    { o.transaction = maps.Clone(o.current) }
func (o *modelObservations) commit()   { o.transaction = nil }
func (o *modelObservations) rollback() { o.current, o.transaction = o.transaction, nil }

func (o *modelObservations) observe(message *warpapi.Message, event, action, ordinal int) {
	id := message.GetId()
	if o.inherited[id] {
		return
	}
	used := message.GetModelUsed()
	if used == nil || strings.TrimSpace(used.GetModelId()) == "" {
		delete(o.current, id)
		return
	}
	if previous, ok := o.current[id]; ok && previous.ID == used.GetModelId() && previous.Fallback == used.GetIsFallback() {
		return // Replayed or unrelated masked updates are not a new model report.
	}
	o.current[id] = observedModelMismatch{ID: used.GetModelId(), Fallback: used.GetIsFallback(), Event: event, Action: action, Message: ordinal}
}

func (o *modelObservations) latest() *observedModelMismatch {
	var latest *observedModelMismatch
	for _, value := range o.current {
		v := value
		if latest == nil || v.Event > latest.Event || (v.Event == latest.Event && (v.Action > latest.Action || (v.Action == latest.Action && v.Message > latest.Message))) {
			latest = &v
		}
	}
	return latest
}

func (c *Client) recordObservedModel(ctx context.Context, requested string, observed *observedModelMismatch) *modelMismatchDiagnostic {
	c.mu.Lock()
	details := modelMismatchDetails(requested, observed, c.account.WarpModelIDs)
	c.mu.Unlock()
	if details != nil {
		if details.ReportedModelKnown {
			upstream.NoteServedModelWithSource(ctx, details.ReportedModel, "warp.model_used")
		} else {
			upstream.NoteRedactedServedModel(ctx, "warp.model_used")
		}
		upstream.NoteModelFallback(ctx, details.Fallback)
	}
	return details
}
