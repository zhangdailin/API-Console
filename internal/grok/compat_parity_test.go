package grok

import (
	"github.com/goccy/go-json"
	"testing"

	"orchids-api/internal/config"
)

func TestResolveModel_RejectsRemovedAliases(t *testing.T) {
	for _, id := range []string{"gork-4.20-0309", "grok-4-1-thinking", "grok-imagine-1.0"} {
		if _, ok := ResolveModel(id); ok {
			t.Fatalf("ResolveModel(%s) should fail", id)
		}
	}
}

func TestResolveModel_Grok42AliasesRejected(t *testing.T) {
	if _, ok := ResolveModel("grok-4.2"); ok {
		t.Fatalf("ResolveModel(grok-4.2) should fail")
	}
	if _, ok := ResolveModel("grok-4-2"); ok {
		t.Fatalf("ResolveModel(grok-4-2) should fail")
	}
}

func TestResolveModel_Grok420Rejected(t *testing.T) {
	if _, ok := ResolveModel("grok-420"); ok {
		t.Fatalf("ResolveModel(grok-420) should fail")
	}
}

func TestResolveModel_CurrentBuildMappings(t *testing.T) {
	cases := []struct {
		modelID      string
		wantUpstream string
	}{
		{modelID: "grok-4.5", wantUpstream: "grok-4.5"},
		{modelID: "grok-4.6", wantUpstream: "grok-4.6"},
	}
	for _, tc := range cases {
		spec, ok := ResolveModel(tc.modelID)
		if !ok {
			t.Fatalf("ResolveModel(%s) should succeed", tc.modelID)
		}
		if spec.UpstreamModel != tc.wantUpstream {
			t.Fatalf("%s upstream=%q want=%q", tc.modelID, spec.UpstreamModel, tc.wantUpstream)
		}
	}
}

func TestResolveModel_LegacyWebsiteModelsDeprecated(t *testing.T) {
	for _, id := range []string{
		"grok-4.20-0309",
		"grok-4.20-0309-non-reasoning-super",
		"grok-4.20-0309-super",
		"grok-4.20-0309-reasoning-super",
		"grok-4.20-0309-non-reasoning-heavy",
		"grok-4.20-0309-heavy",
		"grok-4.20-0309-reasoning-heavy",
		"grok-4.20-fast",
		"grok-4.20-auto",
		"grok-4.20-expert",
		"grok-4.20-heavy",
		"grok-4.3-beta",
	} {
		if _, ok := ResolveModel(id); ok {
			t.Fatalf("ResolveModel(%s) should be removed", id)
		}
		if !IsDeprecatedModelID(id) {
			t.Fatalf("%s should be deprecated", id)
		}
	}
}

func TestResolveModel_AcceptsCurrentBuildModels(t *testing.T) {
	for _, id := range []string{
		"grok-4.5",
		"grok-4.6",
	} {
		if _, ok := ResolveModel(id); !ok {
			t.Fatalf("ResolveModel(%s) should succeed", id)
		}
	}
}

func TestGrok45RoutesToBuildCLI(t *testing.T) {
	spec, ok := ResolveModel("grok-4.5")
	if !ok {
		t.Fatal("ResolveModel(grok-4.5) = false, want true")
	}
	if !modelRoutedToCLI(spec, &config.Config{}) {
		t.Fatal("grok-4.5 should route through the official Build CLI OAuth path")
	}
}

func TestLegacyCLIModelListCannotRouteImplicitModel(t *testing.T) {
	var cfg config.Config
	if err := json.Unmarshal([]byte(`{"grok_cli_model_ids":["implicit-model"]}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if modelRoutedToCLI(ModelSpec{ID: "implicit-model"}, &cfg) {
		t.Fatal("legacy model list must not route models without explicit Build capability")
	}
	if !modelRoutedToCLI(ModelSpec{ID: "dynamic-build", Upstream: UpstreamCLI}, &cfg) {
		t.Fatal("dynamically discovered Build models must stay routed to CLI")
	}
}

func TestResolveModel_Grok420BetaHyphenAliasRejected(t *testing.T) {
	if _, ok := ResolveModel("grok-4-20-beta"); ok {
		t.Fatalf("ResolveModel(grok-4-20-beta) should fail")
	}
}

func TestResolveModel_RejectsUnknownImagineModel(t *testing.T) {
	if _, ok := ResolveModel("grok-imagine-2.0"); ok {
		t.Fatalf("ResolveModel(grok-imagine-2.0) should fail")
	}
}

func TestChatCompletionsRequestValidate_LeavesSamplingToUpstream(t *testing.T) {
	req := ChatCompletionsRequest{
		Model: "grok-4.20-0309",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "hello",
		}},
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
	if req.Temperature != nil {
		t.Fatalf("temperature default mismatch: got=%v", req.Temperature)
	}
	if req.TopP != nil {
		t.Fatalf("top_p default mismatch: got=%v", req.TopP)
	}
}

func TestChatCompletionsRequestValidate_ToolChoice(t *testing.T) {
	req := ChatCompletionsRequest{
		Model: "grok-4.20-0309",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "hello",
		}},
		Tools: []ToolDef{{
			Type: "function",
			Function: map[string]interface{}{
				"name": "weather",
			},
		}},
		ToolChoice: "required",
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}

	req.ToolChoice = "bad-choice"
	if err := req.Validate(); err == nil {
		t.Fatalf("expected invalid tool_choice error")
	}

	req.ToolChoice = map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name": "weather",
		},
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() object tool_choice error: %v", err)
	}

	req.ToolChoice = map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name": "unknown_tool",
		},
	}
	if err := req.Validate(); err == nil {
		t.Fatalf("expected tool_choice function reference error")
	}

	req.ToolChoice = map[string]interface{}{
		"type":     "function",
		"function": map[string]interface{}{},
	}
	if err := req.Validate(); err == nil {
		t.Fatal("expected malformed forced tool_choice error")
	}

	req.Tools = nil
	req.ToolChoice = "required"
	if err := req.Validate(); err == nil {
		t.Fatal("expected required tool_choice without tools error")
	}
}

func TestChatCompletionsRequest_UnmarshalLooseTypes(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.20-0309",
		"messages":[{"role":"user","content":"hello"}],
		"stream":"true",
		"temperature":"1.2",
		"top_p":"0.6"
	}`)
	var req ChatCompletionsRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if req.Stream != true {
		t.Fatalf("stream=%v want=true", req.Stream)
	}
	if req.Temperature == nil || *req.Temperature != 1.2 {
		t.Fatalf("temperature=%v want=1.2", req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0.6 {
		t.Fatalf("top_p=%v want=0.6", req.TopP)
	}
}

func TestChatCompletionsRequest_UnmarshalInvalidStream(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.20-0309",
		"messages":[{"role":"user","content":"hello"}],
		"stream":"maybe"
	}`)
	var req ChatCompletionsRequest
	if err := json.Unmarshal(raw, &req); err == nil {
		t.Fatalf("expected stream parse error")
	}
}

func TestChatCompletionsRequest_StreamProvidedFlagAndDefault(t *testing.T) {
	rawNoStream := []byte(`{
		"model":"grok-4.20-0309",
		"messages":[{"role":"user","content":"hello"}]
	}`)
	var req ChatCompletionsRequest
	if err := json.Unmarshal(rawNoStream, &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if req.StreamProvided {
		t.Fatalf("stream should be marked as not provided")
	}

	cfg := &config.Config{}
	config.ApplyDefaults(cfg)
	h := &Handler{cfg: cfg}
	h.applyDefaultChatStream(&req)
	if req.Stream != true {
		t.Fatalf("default stream=%v want=true", req.Stream)
	}

	rawWithStream := []byte(`{
		"model":"grok-4.20-0309",
		"messages":[{"role":"user","content":"hello"}],
		"stream":false
	}`)
	var req2 ChatCompletionsRequest
	if err := json.Unmarshal(rawWithStream, &req2); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if !req2.StreamProvided {
		t.Fatalf("stream should be marked as provided")
	}
	h.applyDefaultChatStream(&req2)
	if req2.Stream != false {
		t.Fatalf("explicit stream should not be overridden, got=%v", req2.Stream)
	}

	rawWithNullStream := []byte(`{
		"model":"grok-4.20-0309",
		"messages":[{"role":"user","content":"hello"}],
		"stream":null
	}`)
	var req3 ChatCompletionsRequest
	if err := json.Unmarshal(rawWithNullStream, &req3); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if req3.StreamProvided {
		t.Fatalf("stream=null should be treated as not provided")
	}
	h.applyDefaultChatStream(&req3)
	if req3.Stream != true {
		t.Fatalf("null stream should fallback to default true, got=%v", req3.Stream)
	}
}

func TestApplyDefaultChatStream_StreamFalseOverride(t *testing.T) {
	stream := false
	h := &Handler{
		cfg: &config.Config{
			Stream: &stream,
		},
	}
	req := ChatCompletionsRequest{
		StreamProvided: false,
	}
	h.applyDefaultChatStream(&req)
	if req.Stream != false {
		t.Fatalf("stream=%v want=false (config override)", req.Stream)
	}
}
