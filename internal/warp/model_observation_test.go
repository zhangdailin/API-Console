package warp

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
)

func observedModelMessage(id, model string, fallback bool) *warpapi.Message {
	return warpapi.Message_builder{Id: proto.String(id), TaskId: proto.String("root"), ModelUsed: warpapi.Message_ModelUsed_builder{ModelId: proto.String(model), IsFallback: proto.Bool(fallback)}.Build()}.Build()
}

func TestWarpReportsMatchedFallbackUnknownAndAbsentModels(t *testing.T) {
	for _, tc := range []struct {
		name, model, wantStatus string
		fallback                bool
	}{
		{"matched", "model-a", "reported", false},
		{"fallback", "model-b", "reported", true},
		{"unknown", "private-unadvertised-model", "redacted", true},
		{"absent", "", "unreported", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := testAccount()
			account.WarpModelIDs = []string{"model-a", "model-b"}
			c := NewFromAccount(account, testConfig("https://private.invalid"))
			messages := []*warpapi.Message{continuationText("answer", "OK")}
			if tc.model != "" {
				messages = append([]*warpapi.Message{observedModelMessage("model", tc.model, tc.fallback)}, messages...)
			}
			stream := integrityStream(t, compatRootTask("root", messages...), testDone())
			calls := 0
			c.http = &http.Client{Transport: diagnosticRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
			})}
			facts := map[string]string{}
			ctx, capture := debug.WithCapture(upstream.WithRouteObserver(t.Context(), func(k, v string) { facts[k] = v }), "synthetic-model-observation")
			if err := c.SendRequestWithPayload(ctx, testRequest(), nil, nil); err != nil || calls != 1 {
				t.Fatalf("model observation changed generation: %v", err)
			}
			if facts[upstream.RouteModelStatus] != tc.wantStatus || facts[upstream.RouteSentModel] != "model-a" {
				t.Fatal("response identity status or sent identity incorrect")
			}
			if tc.wantStatus == "reported" && (facts[upstream.RouteServedModel] != tc.model || facts[upstream.RouteModelSource] != "warp.model_used") {
				t.Fatal("reported model lost")
			}
			if tc.wantStatus != "reported" && facts[upstream.RouteServedModel] != "" {
				t.Fatal("absent or private model replaced with request alias")
			}
			if tc.model != "" && facts[upstream.RouteModelFallback] != map[bool]string{true: "true", false: "false"}[tc.fallback] {
				t.Fatal("fallback observation lost")
			}
			sections := capture.Bundle().Sections
			var diagnostic generationDiagnostic
			if len(sections) != 1 || json.Unmarshal([]byte(sections[0].Payload), &diagnostic) != nil {
				t.Fatal("model diagnostic missing")
			}
			if (diagnostic.ModelUsed != nil) != (tc.model != "") {
				t.Fatal("matched/absent metadata conflated")
			}
			if tc.wantStatus == "redacted" && strings.Contains(sections[0].Payload, tc.model) {
				t.Fatal("unadvertised model leaked")
			}
		})
	}
}

func TestWarpModelObservationUsesLatestEffectiveCommittedMessage(t *testing.T) {
	begin := testAction(warpapi.ClientAction_builder{BeginTransaction: warpapi.ClientAction_BeginTransaction_builder{}.Build()}.Build())
	rollback := testAction(warpapi.ClientAction_builder{RollbackTransaction: warpapi.ClientAction_RollbackTransaction_builder{}.Build()}.Build())
	commit := testAction(warpapi.ClientAction_builder{CommitTransaction: warpapi.ClientAction_CommitTransaction_builder{}.Build()}.Build())
	for _, end := range []*warpapi.ResponseEvent{rollback, commit} {
		stream := integrityStream(t, compatRootTask("root", observedModelMessage("first", "model-a", false)), begin, integrityTaskMessages(observedModelMessage("later", "model-b", true)), end, integrityTaskMessages(continuationText("answer", "OK")), testDone())
		result, err := readStream(strings.NewReader(stream), "model-a")
		want := "model-a"
		if end == commit {
			want = "model-b"
		}
		if err != nil || result.ModelUsed == nil || result.ModelUsed.ID != want {
			t.Fatalf("model transaction observation: %v", err)
		}
	}
	update := testAction(warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{TaskId: proto.String("root"), Message: observedModelMessage("model", "unselected-private-id", true), Mask: &fieldmaskpb.FieldMask{Paths: []string{"model_used.is_fallback"}}}.Build()}.Build())
	stream := integrityStream(t, compatRootTask("root", observedModelMessage("model", "model-a", false)), update, integrityTaskMessages(continuationText("answer", "OK")), testDone())
	result, err := readStream(strings.NewReader(stream), "model-a")
	if err != nil || result.ModelUsed == nil || result.ModelUsed.ID != "model-a" || !result.ModelUsed.Fallback {
		t.Fatalf("masked model projection: %v", err)
	}
}

func TestWarpContinuationCannotReuseInheritedModelAsCurrentEvidence(t *testing.T) {
	state := newContinuationAccumulator(nil)
	continuationApply(t, state, continuationCreate(observedModelMessage("old-model", "model-b", true)))
	initial, err := state.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	stream := integrityStream(t, integrityTaskMessages(observedModelMessage("old-model", "model-b", true), continuationText("answer", "OK")), testDone())
	result, err := readStreamWithContext(strings.NewReader(stream), "model-a", initial)
	if err != nil || result.ModelUsed != nil {
		t.Fatalf("inherited model presented as new evidence: %v", err)
	}
}
