package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestBuildProbeRequestRoundTrip(t *testing.T) {
	raw, err := buildProbeRequest("probe-model")
	if err != nil {
		t.Fatal(err)
	}
	raw2, err := buildProbeRequest("probe-model")
	if err != nil || !bytes.Equal(raw, raw2) {
		t.Fatal("request must be deterministic")
	}
	var request warpapi.Request
	if err := proto.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	if request.GetSettings().GetModelConfig().GetBase() != "probe-model" {
		t.Fatal("wrong model")
	}
	inputs := request.GetInput().GetUserInputs().GetInputs()
	if len(inputs) != 1 || inputs[0].GetUserQuery().GetQuery() != "Reply exactly OK." {
		t.Fatal("wrong fixed input")
	}
	if inputs[0].GetUserQuery().GetIntendedAgent() != warpapi.AgentType_AGENT_TYPE_PRIMARY {
		t.Fatal("wrong agent")
	}
	if request.GetMcpContext() != nil || len(request.GetTaskContext().GetTasks()) != 0 || request.GetMetadata().GetConversationId() != "" {
		t.Fatal("request contains extra context")
	}
	context := request.GetInput().GetContext()
	if context.GetDirectory().GetPwd() != "" || context.GetDirectory().GetHome() != "" || context.GetShell().GetName() != "sh" || context.GetOperatingSystem().GetPlatform() != "Linux" || context.HasCurrentTime() {
		t.Fatal("context is not synthetic")
	}
	settings := request.GetSettings()
	if settings.GetModelConfig().HasCliAgent() || settings.GetModelConfig().HasComputerUseAgent() {
		t.Fatal("subagent models must not be populated")
	}
	if len(raw) > 1024 {
		t.Fatal("minimal request unexpectedly expanded")
	}
	// Every bool in the official schema must be explicitly present and false.
	fields := settings.ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		if field.Kind() == protoreflect.BoolKind {
			if !settings.ProtoReflect().Has(field) || settings.ProtoReflect().Get(field).Bool() {
				t.Errorf("capability %s not disabled", field.Name())
			}
		}
	}
	for _, tools := range [][]warpapi.ToolType{settings.GetSupportedTools(), settings.GetSupportedCliAgentTools()} {
		if len(tools) != 1 || tools[0] != warpapi.ToolType_SUGGEST_PROMPT {
			t.Fatal("missing non-executable tool fence")
		}
	}
	if settings.GetIsolationLevel() != warpapi.IsolationLevel_SANDBOX || settings.GetAutonomyLevel() != warpapi.AutonomyLevel_SUPERVISED {
		t.Fatal("wrong execution posture")
	}
	for _, model := range []string{"", " model", "model\n", "model\x00", string([]byte{0xff}), strings.Repeat("m", 257)} {
		if _, err := buildProbeRequest(model); err == nil {
			t.Errorf("accepted invalid model %q", model)
		}
	}
}

func probeTestSSE(t *testing.T, event *warpapi.ResponseEvent) string {
	t.Helper()
	raw, err := proto.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + base64.RawURLEncoding.EncodeToString(raw) + "\n\n"
}

func probeTestDone() *warpapi.ResponseEvent {
	return warpapi.ResponseEvent_builder{Finished: warpapi.ResponseEvent_StreamFinished_builder{Done: warpapi.ResponseEvent_StreamFinished_Done_builder{}.Build()}.Build()}.Build()
}

func probeTestOutput(id, text string) *warpapi.Message {
	return warpapi.Message_builder{Id: &id, AgentOutput: warpapi.Message_AgentOutput_builder{Text: &text}.Build()}.Build()
}

func probeTestAction(action *warpapi.ClientAction) *warpapi.ResponseEvent {
	return warpapi.ResponseEvent_builder{ClientActions: warpapi.ResponseEvent_ClientActions_builder{Actions: []*warpapi.ClientAction{action}}.Build()}.Build()
}

func TestReadProbeStreamTextAndCompletion(t *testing.T) {
	first := probeTestAction(warpapi.ClientAction_builder{AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{Messages: []*warpapi.Message{probeTestOutput("m", "O")}}.Build()}.Build())
	appendEvent := probeTestAction(warpapi.ClientAction_builder{AppendToMessageContent: warpapi.ClientAction_AppendToMessageContent_builder{Message: probeTestOutput("m", "K")}.Build()}.Build())
	final := probeTestAction(warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{Message: probeTestOutput("m", "OK")}.Build()}.Build())
	stream := ": heartbeat\n\n" + probeTestSSE(t, first) + probeTestSSE(t, appendEvent) + probeTestSSE(t, final) + probeTestSSE(t, probeTestDone())
	result, err := readProbeStream(strings.NewReader(strings.ReplaceAll(stream, "\n", "\r\n")))
	if err != nil || result != (streamResult{TextBytes: 2, Complete: true, Events: 4}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestReadProbeStreamSnapshotAndUTF8(t *testing.T) {
	message := probeTestOutput("unicode", "好")
	event := probeTestAction(warpapi.ClientAction_builder{CreateTask: warpapi.ClientAction_CreateTask_builder{Task: warpapi.Task_builder{Messages: []*warpapi.Message{message}}.Build()}.Build()}.Build())
	result, err := readProbeStream(strings.NewReader(probeTestSSE(t, event) + probeTestSSE(t, probeTestDone())))
	if err != nil || result.TextBytes != 3 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestReadProbeStreamSplitAndUnterminatedFrame(t *testing.T) {
	raw, err := proto.Marshal(probeTestDone())
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.URLEncoding.EncodeToString(raw)
	stream := "event: message\nid: synthetic\ndata: " + encoded[:2] + "\ndata: " + encoded[2:]
	result, err := readProbeStream(strings.NewReader(stream))
	if err != nil || !result.Complete || result.Events != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestReadProbeStreamRequiresExplicitSuccess(t *testing.T) {
	output := probeTestAction(warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{Message: probeTestOutput("m", "OK")}.Build()}.Build())
	for _, stream := range []string{"", ": heartbeat\n\n", probeTestSSE(t, output), "event: completed\n\n", "event: task_end\n\n"} {
		result, err := readProbeStream(strings.NewReader(stream))
		if !errors.Is(err, errProbeIncomplete) || result.Complete {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
}

func TestReadProbeStreamFixedErrors(t *testing.T) {
	secret := "upstream prose must not escape"
	upstreamError := warpapi.ResponseEvent_builder{Finished: warpapi.ResponseEvent_StreamFinished_builder{InternalError: warpapi.ResponseEvent_StreamFinished_InternalError_builder{Message: &secret}.Build()}.Build()}.Build()
	quota := warpapi.ResponseEvent_builder{Finished: warpapi.ResponseEvent_StreamFinished_builder{QuotaLimit: warpapi.ResponseEvent_StreamFinished_QuotaLimit_builder{}.Build()}.Build()}.Build()
	for _, stream := range []string{"event: error\ndata: " + secret + "\n\n", "event: error\n\n", probeTestSSE(t, upstreamError), probeTestSSE(t, quota)} {
		result, err := readProbeStream(strings.NewReader(stream))
		if !errors.Is(err, errProbeUpstream) || result.Complete || strings.Contains(err.Error(), secret) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
	_, err := readProbeStream(probeTestErrorReader{})
	if !errors.Is(err, errProbeStream) {
		t.Fatalf("unexpected reader error: %v", err)
	}
}

type probeTestErrorReader struct{}

func (probeTestErrorReader) Read([]byte) (int, error) { return 0, errors.New("private reader detail") }

func TestReadProbeStreamRejectsToolRequests(t *testing.T) {
	for _, message := range []*warpapi.Message{
		warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{}.Build()}.Build(),
		warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{Server: warpapi.Message_ToolCall_Server_builder{}.Build()}.Build()}.Build(),
		warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{SuggestPrompt: warpapi.Message_ToolCall_SuggestPrompt_builder{}.Build()}.Build()}.Build(),
		warpapi.Message_builder{WebSearch: warpapi.Message_WebSearch_builder{}.Build()}.Build(),
		warpapi.Message_builder{WebFetch: warpapi.Message_WebFetch_builder{}.Build()}.Build(),
		warpapi.Message_builder{InvokeSkill: warpapi.Message_InvokeSkill_builder{}.Build()}.Build(),
	} {
		event := probeTestAction(warpapi.ClientAction_builder{AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{Messages: []*warpapi.Message{message}}.Build()}.Build())
		result, err := readProbeStream(strings.NewReader(probeTestSSE(t, event) + probeTestSSE(t, probeTestDone())))
		if !errors.Is(err, errProbeTool) || !result.ToolRequested || result.Complete {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
}

func TestReadProbeStreamMalformed(t *testing.T) {
	for _, stream := range []string{"data: !!!\n\n", "data: \n\n", "data: [DONE]\n\n", "data: _w\n\n", "data: CAE\n\n"} {
		result, err := readProbeStream(strings.NewReader(stream))
		if !errors.Is(err, errProbeStream) || result.Complete {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
}

func TestReadProbeStreamLimits(t *testing.T) {
	for name, stream := range map[string]string{
		"line":  ":" + strings.Repeat("a", probeFrameLimit) + "\n\n",
		"frame": strings.Repeat(":"+strings.Repeat("a", 1023)+"\n", 1024),
		"total": strings.Repeat(":"+strings.Repeat("a", 1020)+"\n\n", probeStreamLimit/1023+1),
	} {
		t.Run(name, func(t *testing.T) {
			result, err := readProbeStream(strings.NewReader(stream))
			if err != errProbeLimit || result.Complete {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
	// Exactly the total limit is permitted; without completion it is incomplete.
	stream := strings.Repeat(":\n\n", probeStreamLimit/3) + strings.Repeat("\n", probeStreamLimit%3)
	_, err := readProbeStream(strings.NewReader(stream))
	if err != errProbeIncomplete {
		t.Fatalf("exact limit: %v", err)
	}
}

func TestReadProbeStreamRollbackDiscardsText(t *testing.T) {
	begin := probeTestAction(warpapi.ClientAction_builder{BeginTransaction: warpapi.ClientAction_BeginTransaction_builder{}.Build()}.Build())
	add := probeTestAction(warpapi.ClientAction_builder{AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{Messages: []*warpapi.Message{probeTestOutput("m", "OK")}}.Build()}.Build())
	rollback := probeTestAction(warpapi.ClientAction_builder{RollbackTransaction: warpapi.ClientAction_RollbackTransaction_builder{}.Build()}.Build())
	commit := probeTestAction(warpapi.ClientAction_builder{CommitTransaction: warpapi.ClientAction_CommitTransaction_builder{}.Build()}.Build())
	result, err := readProbeStream(strings.NewReader(probeTestSSE(t, begin) + probeTestSSE(t, add) + probeTestSSE(t, rollback) + probeTestSSE(t, probeTestDone())))
	if err != nil || result.TextBytes != 0 {
		t.Fatalf("rolled-back text counted: result=%+v err=%v", result, err)
	}
	result, err = readProbeStream(strings.NewReader(probeTestSSE(t, begin) + probeTestSSE(t, add) + probeTestSSE(t, commit) + probeTestSSE(t, probeTestDone())))
	if err != nil || result.TextBytes != 2 || !result.Complete {
		t.Fatalf("committed text lost: result=%+v err=%v", result, err)
	}
}

func TestReadProbeStreamEmptyReplacementRemovesText(t *testing.T) {
	first := probeTestAction(warpapi.ClientAction_builder{AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{Messages: []*warpapi.Message{probeTestOutput("m", "OK")}}.Build()}.Build())
	last := probeTestAction(warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{Message: probeTestOutput("m", "")}.Build()}.Build())
	result, err := readProbeStream(strings.NewReader(probeTestSSE(t, first) + probeTestSSE(t, last) + probeTestSSE(t, probeTestDone())))
	if err != nil || result.TextBytes != 0 {
		t.Fatal("removed answer counted as usable output")
	}
}

func TestReadProbeStreamStopsAtCompletion(t *testing.T) {
	reader := io.MultiReader(strings.NewReader(probeTestSSE(t, probeTestDone())), probeTestErrorReader{})
	result, err := readProbeStream(reader)
	if err != nil || !result.Complete {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
