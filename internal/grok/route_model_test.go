package grok

import (
	"testing"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/upstream"
)

func TestResponseModelObservationDoesNotDependOnProviderAuditLogger(t *testing.T) {
	facts := map[string]string{}
	ctx := upstream.WithRouteObserver(t.Context(), func(k, v string) { facts[k] = v })
	h := &Handler{}
	h.auditChatOutcome(ctx, nil, &chatwire.Request{Model: "requested"}, chatOutcome{ServedModel: "actual-response-model"})
	if facts[upstream.RouteServedModel] != "actual-response-model" || facts[upstream.RouteModelSource] != "responses.model" {
		t.Fatal("response metadata depended on a separate audit sink")
	}
}
