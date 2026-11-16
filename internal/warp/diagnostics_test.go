package warp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/upstream"
)

type diagnosticRoundTripper func(*http.Request) (*http.Response, error)

func (f diagnosticRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestGenerationStageErrorUsesOnlyFixedFields(t *testing.T) {
	for _, tc := range []struct {
		stage    string
		cause    error
		category int
	}{
		{"transport", nil, 0},
		{"http_status", nil, 0},
		{"content_type", nil, 0},
		{"stream_protocol", errStreamStream, 1},
		{"tool_or_subagent", errStreamTool, 2},
		{"incomplete", errStreamIncomplete, 3},
		{"finish", errStreamFinish, 4},
		{"finish", errStreamUpstream, 4},
		{"limit", errStreamLimit, 5},
		{"model_substitution", errStreamModelSubstitution, 6},
		{"usage_invalid", errUsage, 7},
	} {
		t.Run(fmt.Sprintf("%s_%d", tc.stage, tc.category), func(t *testing.T) {
			cause := tc.cause
			if cause != nil {
				cause = fmt.Errorf("private-body-and-credential: %w", cause)
				stage, category := generationParserFailure(cause)
				if stage != tc.stage || category != tc.category {
					t.Fatalf("wrapped parser sentinel not classified: %s %d", stage, category)
				}
			}
			err := generationStageError(tc.stage, 200, cause)
			var app *apperrors.AppError
			if !errors.As(err, &app) || app.HTTPStatus != 502 || app.Code != "warp_generation_failed" {
				t.Fatalf("not a fixed 502: %v", err)
			}
			if app.Message != "Warp generation failed at "+tc.stage+"; no automatic retry" || len(app.Details) != 3 || app.Details["stage"] != tc.stage || app.Details["upstream_http_status"] != 200 || app.Details["parser_category"] != tc.category {
				t.Fatalf("wrong fixed diagnostics: %+v", app)
			}
			if strings.Contains(err.Error()+string(app.ToJSON()), "private-") || errors.Unwrap(err) != nil {
				t.Fatal("raw cause escaped the fixed error")
			}
		})
	}
	var app *apperrors.AppError
	err := generationStageError("https://private.invalid/token", 999, errors.New("private-parser-body"))
	if !errors.As(err, &app) || app.Details["stage"] != "stream_protocol" || app.Details["upstream_http_status"] != 0 || strings.Contains(string(app.ToJSON()), "private") {
		t.Fatal("untrusted stage or status escaped normalization")
	}
}

func TestGenerationContentTypeAllowlist(t *testing.T) {
	for raw, want := range map[string]string{
		"":                                 "missing",
		"TEXT/EVENT-STREAM; charset=utf-8": "text/event-stream",
		"application/json; token=private":  "application/json",
		"text/html":                        "text/html",
		"text/plain":                       "text/plain",
		"application/octet-stream":         "application/octet-stream",
		"application/private-token":        "other",
		"https://private.invalid":          "other",
		strings.Repeat("x", 257):           "other",
	} {
		if got := generationContentType(raw); got != want {
			t.Fatalf("content type normalization got %q, want %q", got, want)
		}
	}
}

func TestGenerationDiagnosticsNeverCaptureWireOrCredentials(t *testing.T) {
	const private = "opaque-private-echo-value"
	output := testSSE(t, testMessages(testOutput("private-message-id", private)))
	tool := testSSE(t, testMessages(warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{}.Build()}.Build()))
	for _, tc := range []struct {
		name, contentType, body, stage      string
		status, events, textBytes, category int
		transportErr                        error
	}{
		{name: "transport", stage: "transport", transportErr: errors.New("https://private.invalid/" + private)},
		{name: "HTTP error echoes token", status: 403, contentType: "application/json; token=" + private, body: private, stage: "http_status"},
		{name: "content type echoes token", status: 200, contentType: "application/" + private, body: private, stage: "content_type"},
		{name: "protocol echoes token", status: 200, contentType: "text/event-stream", body: "data: " + private + "\n\n", stage: "stream_protocol", category: 1},
		{name: "upstream error echoes token", status: 200, contentType: "text/event-stream", body: "event: error\ndata: " + private + "\n\n", stage: "finish", events: 1, category: 4},
		{name: "partial then tool", status: 200, contentType: "text/event-stream", body: output + tool, stage: "tool_or_subagent", events: 2, textBytes: len(private), category: 2},
		{name: "partial incomplete", status: 200, contentType: "text/event-stream", body: output, stage: "incomplete", events: 1, textBytes: len(private), category: 3},
		{name: "success metadata only", status: 200, contentType: "text/event-stream; charset=utf-8", body: output + testSSE(t, testDone()), stage: "success", events: 2, textBytes: len(private)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			calls := 0
			account := testAccount()
			account.WarpAccessToken = private
			client := NewFromAccount(account, testConfig("https://private.invalid"))
			client.http = &http.Client{Transport: diagnosticRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Path != "/ai/multi-agent" || req.Header.Get("Authorization") != "Bearer "+private {
					t.Fatal("generation gates or request changed")
				}
				if tc.transportErr != nil {
					return nil, tc.transportErr
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: req}, nil
			})}
			ctx, capture := debug.WithCapture(context.Background(), "safe-request-id")
			emitted := 0
			err := client.SendRequestWithPayload(ctx, testRequest(), func(upstream.SSEMessage) { emitted++ }, nil)
			if calls != 1 {
				t.Fatalf("generation retried: %d calls", calls)
			}
			var public string
			if tc.stage == "success" {
				if err != nil || emitted != 2 || logs.Len() != 0 {
					t.Fatalf("success not emitted cleanly: err=%v emitted=%d", err, emitted)
				}
			} else {
				var app *apperrors.AppError
				if !errors.As(err, &app) || app.Details["stage"] != tc.stage || emitted != 0 {
					t.Fatalf("unsafe generation failure: %v emitted=%d", err, emitted)
				}
				public = err.Error() + string(app.ToJSON())
				var record map[string]any
				if json.Unmarshal(logs.Bytes(), &record) != nil || len(record) != 6 || record["stage"] != tc.stage || record["upstream_http_status"] != float64(tc.status) || record["parser_category"] != float64(tc.category) || record["level"] != "WARN" {
					t.Fatalf("log must contain only fixed structured diagnostics: %s", logs.String())
				}
			}
			bundle := capture.Bundle()
			if len(bundle.Sections) != 1 || bundle.Sections[0].Name != "3_warp_diagnostic.json" {
				t.Fatalf("unexpected raw capture sections: %+v", bundle.Sections)
			}
			var diagnostic generationDiagnostic
			if json.Unmarshal([]byte(bundle.Sections[0].Payload), &diagnostic) != nil || diagnostic.Stage != tc.stage || diagnostic.UpstreamHTTPStatus != tc.status || diagnostic.ParserEvents != tc.events || diagnostic.TextBytes != tc.textBytes || diagnostic.ParserCategory != tc.category || diagnostic.ContentType != generationContentType(tc.contentType) {
				t.Fatalf("wrong sanitized diagnostic: %s", bundle.Sections[0].Payload)
			}
			all := public + logs.String() + bundle.Sections[0].Payload
			for _, forbidden := range []string{private, "private.invalid", "private-message-id", "Authorization", "Bearer ", "data:", "/ai/multi-agent", "What is a test?"} {
				if strings.Contains(all, forbidden) {
					t.Fatalf("private wire metadata escaped via diagnostics: %q", forbidden)
				}
			}
		})
	}
}

func TestGenerationDiagnosticUsageContainsNumbersOnly(t *testing.T) {
	input, credits := uint64(17), float32(0.25)
	finish := warpapi.ResponseEvent_StreamFinished_builder{
		Done: warpapi.ResponseEvent_StreamFinished_Done_builder{}.Build(),
		RequestCharges: warpapi.RequestCharges_builder{UsageByCategory: map[string]*warpapi.ChargedUsage{
			"private-category": warpapi.ChargedUsage_builder{DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{
				"private-model": warpapi.InferenceUsage_builder{TokenCount: warpapi.TokenCount_builder{Input: &input}.Build(), TokenCost: warpapi.TokenCost_builder{InputCostInCredits: &credits}.Build()}.Build(),
			}}.Build(),
		}}.Build(),
	}.Build()
	stream := testSSE(t, testMessages(testOutput("m", "private-answer"))) + testSSE(t, warpapi.ResponseEvent_builder{Finished: finish}.Build())
	client := NewFromAccount(testAccount(), testConfig("https://private.invalid"))
	client.http = &http.Client{Transport: diagnosticRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	})}
	ctx, capture := debug.WithCapture(context.Background(), "safe-request-id")
	if err := client.SendRequestWithPayload(ctx, testRequest(), nil, nil); err != nil {
		t.Fatal(err)
	}
	bundle := capture.Bundle()
	var diagnostic generationDiagnostic
	if len(bundle.Sections) != 1 || json.Unmarshal([]byte(bundle.Sections[0].Payload), &diagnostic) != nil {
		t.Fatal("missing sanitized usage diagnostic")
	}
	if len(diagnostic.Usage) != 2 || diagnostic.Usage["inputTokens"] != float64(17) || diagnostic.Usage["warpInferenceCredits"] != 0.25 {
		t.Fatalf("wrong numeric usage: %+v", diagnostic.Usage)
	}
	if strings.Contains(bundle.Sections[0].Payload, "private") {
		t.Fatal("usage identifiers or response content leaked")
	}
}
