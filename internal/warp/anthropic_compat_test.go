package warp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

// A real Claude Code style request carries output_config.effort, a thinking
// mode, a metadata user id, an Anthropic system string, generation controls and
// Anthropic-shaped tools. Warp can express none of the effort controls: they
// must pass the raw gate, be ignored by the normalized validator, and leave the
// exact model and settings untouched.
func TestAnthropicEffortControlsAreAcceptedButNotForwarded(t *testing.T) {
	const exactModel = "model-a"
	raw := `{"model":"model-a","messages":[{"role":"user","content":"hi"}],"system":"Answer briefly.","output_config":{"effort":"high"},"thinking":{"type":"enabled","budget_tokens":1024},"max_tokens":64,"metadata":{"user_id":"synthetic"},"tools":[{"name":"echo","description":"echo","input_schema":{"type":"object"}}],"stream":false}`
	if err := validateRawRequest(json.RawMessage(raw)); err != nil {
		t.Fatalf("Anthropic compatibility rejected: %v", err)
	}
	// A non-empty ReasoningEffort is produced by the shared handler from the
	// output_config/thinking hints. Because Warp is ExactModelID, validateRequest
	// must ignore it: no model-variant rewrite, no effort forwarded upstream.
	req := upstream.UpstreamRequest{Model: exactModel, ReasoningEffort: "high", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}, Tools: []any{map[string]any{"name": "echo", "description": "echo", "input_schema": map[string]any{"type": "object"}}}}
	prepared, err := validateRequest(req)
	if err != nil {
		t.Fatalf("effort reached normalized validator: %v", err)
	}
	body, err := buildRequest(exactModel, prepared)
	if err != nil {
		t.Fatal(err)
	}
	var decoded warpapi.Request
	if err := proto.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	// The model stays exactly as the caller supplied it.
	modelConfig := decoded.GetSettings().GetModelConfig()
	if modelConfig.GetBase() != exactModel || modelConfig.HasCliAgent() || modelConfig.HasComputerUseAgent() || modelConfig.HasPlanning() || modelConfig.HasCoding() {
		t.Fatalf("effort changed exact model or added a subagent variant: %+v", modelConfig)
	}
	// No effort/thinking control appears anywhere in the settings. Warp's only
	// reasoning-related flag is a capability fence that must remain false.
	settings := decoded.GetSettings()
	if settings == nil {
		t.Fatal("no settings in Warp request")
	}
	if settings.HasSupportsReasoningMessage() && settings.GetSupportsReasoningMessage() {
		t.Fatal("reasoning-message capability must remain disabled")
	}
	if settings.GetCustomModelProviders() != nil {
		t.Fatal("no custom model provider (and hence no reasoning_effort) may be configured")
	}
	fields := settings.ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		name := strings.ToLower(string(field.Name()))
		if strings.Contains(name, "effort") || strings.Contains(name, "reasoning") && field.Kind() != protoreflect.BoolKind {
			t.Errorf("settings field %s leaked a reasoning/effort control", field.Name())
		}
		if field.Kind() == protoreflect.BoolKind && (!settings.ProtoReflect().Has(field) || settings.ProtoReflect().Get(field).Bool()) {
			t.Errorf("Warp capability %s must stay explicitly false", field.Name())
		}
	}
	// The Anthropic-shaped tool survives translation into an MCP declaration.
	servers := decoded.GetMcpContext().GetServers()
	var declaredNames []string
	for _, server := range servers {
		for _, tool := range server.GetTools() {
			declaredNames = append(declaredNames, tool.GetName())
		}
	}
	if len(declaredNames) != 1 || declaredNames[0] != "echo" {
		t.Fatalf("Anthropic tool was not declared as MCP tool: %v", declaredNames)
	}
	if got := decoded.GetInput().GetUserInputs().GetInputs(); len(got) != 1 || got[0].GetUserQuery().GetQuery() != "hi" {
		t.Fatal("original user text was not preserved")
	}
}

func TestAnthropicCompatibilityRejectsUnsupportedAndMalformedControls(t *testing.T) {
	const base = `"model":"model-a","messages":[{"role":"user","content":"hi"}]`
	cases := map[string]string{
		"output_config_empty":         `{"output_config":{}}`,
		"output_config_null":          `{"output_config":null}`,
		"output_config_unknown_key":   `{"output_config":{"effort":"high","verbosity":"low"}}`,
		"output_config_format":        `{"output_config":{"format":{"type":"json_object"}}}`,
		"output_config_format_effort": `{"output_config":{"effort":"high","format":{"type":"json_object"}}}`,
		"output_config_numeric":       `{"output_config":{"effort":1}}`,
		"output_config_empty_effort":  `{"output_config":{"effort":""}}`,
		"output_config_array":         `{"output_config":[]}`,
		"thinking_empty":              `{"thinking":{}}`,
		"thinking_no_type":            `{"thinking":{"budget_tokens":100}}`,
		"thinking_bad_type":           `{"thinking":{"type":"auto"}}`,
		"thinking_unknown_key":        `{"thinking":{"type":"enabled","effort":"high"}}`,
		"thinking_negative_budget":    `{"thinking":{"type":"enabled","budget_tokens":-1}}`,
		"thinking_float_budget":       `{"thinking":{"type":"enabled","budget_tokens":1.5}}`,
		"thinking_string_budget":      `{"thinking":{"type":"enabled","budget_tokens":"1024"}}`,
		"thinking_null_budget":        `{"thinking":{"type":"enabled","budget_tokens":null}}`,
		"metadata_unknown_key":        `{"metadata":{"user_id":"u","session_id":"s"}}`,
		"metadata_numeric_user":       `{"metadata":{"user_id":1}}`,
		"metadata_empty_user":         `{"metadata":{"user_id":""}}`,
		"metadata_array":              `{"metadata":[]}`,
	}
	for name, control := range cases {
		t.Run(name, func(t *testing.T) {
			raw := `{` + base + `,` + strings.Trim(control, "{}") + `}`
			var app *apperrors.AppError
			if err := validateRawRequest(json.RawMessage(raw)); err == nil {
				t.Fatalf("unsupported control accepted: %s", raw)
			} else if !errors.As(err, &app) || app.HTTPStatus != 400 {
				t.Fatalf("unexpected rejection: %v", err)
			}
		})
	}
	for name, raw := range map[string]string{
		"output_config_format_message": `{"model":"model-a","messages":[{"role":"user","content":"hi"}],"output_config":{"format":{"type":"json_object"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := validateRawRequest(json.RawMessage(raw))
			var app *apperrors.AppError
			if !errors.As(err, &app) || app.HTTPStatus != 400 || !strings.Contains(app.Message, "format") {
				t.Fatalf("format must yield a clear 400: %v", err)
			}
		})
	}
}
