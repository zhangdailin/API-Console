package warp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/upstream"
)

func TestModelMismatchAcceptedWithoutReplayAndRedactsUnknownIDs(t *testing.T) {
	for _, tc := range []struct {
		name, id        string
		fallback, known bool
		reason          string
	}{
		{"known-other", "model-b", false, true, "model_id_mismatch"},
		{"same-fallback", "model-a", true, true, "fallback"},
		{"other-fallback", "model-b", true, true, "fallback_and_model_id_mismatch"},
		{"unknown-private", "Bearer synthetic-private-credential", false, false, "model_id_mismatch"},
		{"unknown-alias", "unadvertised-model", false, false, "model_id_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := warpapi.Message_builder{Id: proto.String("model-used"), ModelUsed: warpapi.Message_ModelUsed_builder{ModelId: proto.String(tc.id), IsFallback: proto.Bool(tc.fallback)}.Build()}.Build()
			stream := testSSE(t, compatRootTask("root", model, testOutput("answer", "safe answer"))) + testSSE(t, testDone())
			account := testAccount()
			account.WarpModelIDs = append(account.WarpModelIDs, "model-b")
			c := NewFromAccount(account, testConfig("https://private.invalid"))
			calls := 0
			var events []upstream.SSEMessage
			c.http = &http.Client{Transport: diagnosticRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				raw, err := io.ReadAll(req.Body)
				var wire warpapi.Request
				if err != nil || proto.Unmarshal(raw, &wire) != nil || wire.GetSettings().GetModelConfig().GetBase() != "model-a" {
					t.Fatal("fallback changed the requested wire model")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
			})}
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			ctx, capture := debug.WithCapture(t.Context(), "synthetic-model-mismatch")
			err := c.SendRequestWithPayload(ctx, testRequest(), func(e upstream.SSEMessage) { events = append(events, e) }, nil)
			if err != nil || calls != 1 || len(events) != 2 || events[0].Type != "model.text-delta" || events[0].Event["delta"] != "safe answer" || events[1].Type != "model.finish" {
				t.Fatalf("model fallback did not complete exactly once: %v", err)
			}
			bundle := capture.Bundle()
			if len(bundle.Sections) != 1 {
				t.Fatal("unexpected raw wire capture")
			}
			var d generationDiagnostic
			if json.Unmarshal([]byte(bundle.Sections[0].Payload), &d) != nil || d.ModelMismatch == nil {
				t.Fatal("missing model diagnostic")
			}
			m := d.ModelMismatch
			digest := sha256.Sum256([]byte(tc.id))
			if m.RequestedModel != "model-a" || m.Fallback != tc.fallback || m.ReportedModelKnown != tc.known || m.Reason != tc.reason || m.ReportedModelSHA256 != hex.EncodeToString(digest[:]) || m.ReportedModelBytes != len(tc.id) || m.EventOrdinal != 1 || m.ActionOrdinal != 1 || m.MessageOrdinal != 1 || d.Failure != nil || d.Stage != "success" || d.ParserCategory != 0 {
				t.Fatalf("incorrect model evidence: %+v", m)
			}
			if tc.known && m.ReportedModel != tc.id {
				t.Fatal("known model omitted")
			}
			if !tc.known && (m.ReportedModel != "" || strings.Contains(bundle.Sections[0].Payload+logs.String(), tc.id)) {
				t.Fatal("unknown model payload escaped redaction")
			}
			public, marshalErr := json.Marshal(events)
			if marshalErr != nil || !strings.Contains(logs.String(), `"model_mismatch"`) || !strings.Contains(logs.String(), "Warp accepted upstream model fallback or substitution") || strings.Contains(string(public), tc.id) {
				t.Fatal("diagnostic missing from logs or escaped through public events")
			}
		})
	}
}

func TestModelFallbackDoesNotBypassStreamValidation(t *testing.T) {
	model := warpapi.Message_builder{Id: proto.String("model-used"), ModelUsed: warpapi.Message_ModelUsed_builder{ModelId: proto.String("model-b"), IsFallback: proto.Bool(true)}.Build()}.Build()
	prefix := testSSE(t, compatRootTask("root", model, testOutput("answer", "private partial")))
	for _, tc := range []struct {
		name, suffix, stage string
	}{
		{"incomplete", "", "incomplete"},
		{"malformed", "data: not-protobuf\n\n", "stream_protocol"},
		{"undeclared-tool", testSSE(t, testMessages(warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{}.Build()}.Build())) + testSSE(t, testDone()), "tool_or_subagent"},
		{"frame-limit", strings.Repeat("x", frameLimit+1), "limit"},
		{"invalid-usage", testSSE(t, warpapi.ResponseEvent_builder{Finished: usageTestFinish(warpapi.ChargedUsage_builder{PlatformUsageInCredits: proto.Float32(-1)}.Build())}.Build()), "usage_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewFromAccount(testAccount(), testConfig("https://private.invalid"))
			calls, emitted := 0, 0
			c.http = &http.Client{Transport: diagnosticRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(prefix + tc.suffix))}, nil
			})}
			ctx, capture := debug.WithCapture(t.Context(), "synthetic-fallback-failure")
			err := c.SendRequestWithPayload(ctx, testRequest(), func(upstream.SSEMessage) { emitted++ }, nil)
			var app *apperrors.AppError
			if !errors.As(err, &app) || app.HTTPStatus != http.StatusBadGateway || app.Details["stage"] != tc.stage || calls != 1 || emitted != 0 {
				t.Fatalf("fallback bypassed validation or replayed generation: %v", err)
			}
			bundle := capture.Bundle()
			var d generationDiagnostic
			if len(bundle.Sections) != 1 || json.Unmarshal([]byte(bundle.Sections[0].Payload), &d) != nil || d.ModelMismatch == nil || d.Stage != tc.stage || d.Failure == nil {
				t.Fatal("model discrepancy hid subsequent stream failure diagnostics")
			}
		})
	}
}

func TestModelFallbackPreservesUsageAndFirstDiscrepancy(t *testing.T) {
	first := warpapi.Message_builder{ModelUsed: warpapi.Message_ModelUsed_builder{ModelId: proto.String("model-b"), IsFallback: proto.Bool(true)}.Build()}.Build()
	later := warpapi.Message_builder{ModelUsed: warpapi.Message_ModelUsed_builder{ModelId: proto.String("model-c")}.Build()}.Build()
	finish := usageTestFinish(warpapi.ChargedUsage_builder{
		PlatformUsageInCredits: proto.Float32(1.5),
		DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{"model-b": warpapi.InferenceUsage_builder{
			TokenCount: warpapi.TokenCount_builder{Input: proto.Uint64(12), Output: proto.Uint64(7)}.Build(),
		}.Build()},
	}.Build())
	finish.SetDone(warpapi.ResponseEvent_StreamFinished_Done_builder{}.Build())
	stream := testSSE(t, testMessages(first, testOutput("answer", "safe answer"), later)) + testSSE(t, warpapi.ResponseEvent_builder{Finished: finish}.Build())
	result, err := readStream(strings.NewReader(stream), "model-a")
	if err != nil || !result.Complete || result.Text != "safe answer" || result.ModelMismatch == nil || result.ModelMismatch.ID != "model-b" || !result.ModelMismatch.Fallback {
		t.Fatalf("fallback lost output or first discrepancy: %v", err)
	}
	usage := result.Usage
	if !usage.Measured || usage.InputTokens == nil || *usage.InputTokens != 12 || usage.OutputTokens == nil || *usage.OutputTokens != 7 || usage.PlatformCredits == nil || *usage.PlatformCredits != 1.5 {
		t.Fatal("fallback changed observed usage")
	}
}
