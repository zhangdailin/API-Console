package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
)

const (
	probePrompt      = "Reply exactly OK."
	probeFrameLimit  = 1 << 20
	probeStreamLimit = 8 << 20
)

var (
	errProbeStream     = errors.New("invalid probe stream")
	errProbeLimit      = errors.New("probe stream size limit exceeded")
	errProbeUpstream   = errors.New("probe upstream error")
	errProbeTool       = errors.New("probe requested a tool")
	errProbeIncomplete = errors.New("probe stream ended without completion")
)

type streamResult struct {
	TextBytes     int
	Complete      bool
	ToolRequested bool
	Events        int
	Usage         probeUsage
}

// buildProbeRequest never reads the environment, filesystem, clock, or history.
func buildProbeRequest(model string) ([]byte, error) {
	if validateGenerationPolicy(true, true, model, 0) != nil || strings.TrimSpace(model) != model || len(model) > 256 || !utf8.ValidString(model) || strings.ContainsAny(model, "\r\n\x00") {
		return nil, errors.New("invalid probe model")
	}
	no := false
	empty := ""
	platform, shell := "Linux", "sh"
	autonomy, isolation := warpapi.AutonomyLevel_SUPERVISED, warpapi.IsolationLevel_SANDBOX
	settings := warpapi.Request_Settings_builder{
		ModelConfig:                 warpapi.Request_Settings_ModelConfig_builder{Base: &model}.Build(),
		RulesEnabled:                &no,
		WebContextRetrievalEnabled:  &no,
		SupportsParallelToolCalls:   &no,
		UseAnthropicTextEditorTools: &no,
		PlanningEnabled:             &no,
		WarpDriveContextEnabled:     &no,
		SupportsCreateFiles:         &no,
		// An empty list permits ANY tool. This passive fence advertises no
		// executable tool; suggestions are also disabled explicitly below.
		SupportedTools:                             []warpapi.ToolType{warpapi.ToolType_SUGGEST_PROMPT},
		SupportedCliAgentTools:                     []warpapi.ToolType{warpapi.ToolType_SUGGEST_PROMPT},
		SupportsLongRunningCommands:                &no,
		ShouldPreserveFileContentInHistory:         &no,
		SupportsTodosUi:                            &no,
		SupportsLinkedCodeBlocks:                   &no,
		SupportsStartedChildTaskMessage:            &no,
		SupportsSuggestPrompt:                      &no,
		SupportsReadImageFiles:                     &no,
		SupportsReasoningMessage:                   &no,
		AutonomyLevel:                              &autonomy,
		IsolationLevel:                             &isolation,
		WebSearchEnabled:                           &no,
		SupportsV4AFileDiffs:                       &no,
		SupportsSummarizationViaMessageReplacement: &no,
		SupportsBundledSkills:                      &no,
		SupportsResearchAgent:                      &no,
		SupportsOrchestrationV2:                    &no,
		SupportsBackgroundComputerUse:              &no,
		SupportsOrchestrationRunners:               &no,
		SupportsCreateFileOverwrite:                &no,
		SupportsStoredScreenshots:                  &no,
	}.Build()
	query := probePrompt
	agent := warpapi.AgentType_AGENT_TYPE_PRIMARY
	request := warpapi.Request_builder{
		TaskContext: warpapi.Request_TaskContext_builder{}.Build(),
		Input: warpapi.Request_Input_builder{
			Context: warpapi.InputContext_builder{
				Directory:       warpapi.InputContext_Directory_builder{Pwd: &empty, Home: &empty, PwdFileSymbolsIndexed: &no}.Build(),
				OperatingSystem: warpapi.InputContext_OperatingSystem_builder{Platform: &platform, Distribution: &empty}.Build(),
				Shell:           warpapi.InputContext_Shell_builder{Name: &shell, Version: &empty}.Build(),
			}.Build(),
			UserInputs: warpapi.Request_Input_UserInputs_builder{
				Inputs: []*warpapi.Request_Input_UserInputs_UserInput{
					warpapi.Request_Input_UserInputs_UserInput_builder{
						UserQuery: warpapi.Request_Input_UserQuery_builder{Query: &query, Mode: warpapi.UserQueryMode_builder{}.Build(), IntendedAgent: &agent}.Build(),
					}.Build(),
				},
			}.Build(),
		}.Build(),
		Settings: settings,
		Metadata: warpapi.Request_Metadata_builder{}.Build(),
	}.Build()
	return proto.MarshalOptions{Deterministic: true}.Marshal(request)
}

// readProbeStream retains counters only, never response prose. All errors are
// fixed strings, including reader failures, because upstream data is untrusted.
func readProbeStream(source io.Reader) (streamResult, error) {
	return readProbeStreamForModel(source, "")
}

func readProbeStreamForModel(source io.Reader, expectedModel string) (streamResult, error) {
	var result streamResult
	if source == nil {
		return result, errProbeStream
	}
	limited := &io.LimitedReader{R: source, N: probeStreamLimit + 1}
	reader := bufio.NewReaderSize(limited, 32<<10)
	var frame strings.Builder
	var eventName string
	frameBytes := 0
	hasData := false
	// Only lengths are kept for snapshot de-duplication, not generated text.
	textLengths := make(map[string]int)
	txDepth, txText := 0, 0
	var txLengths map[string]int
	flush := func() error {
		defer func() { frame.Reset(); frameBytes = 0; eventName = ""; hasData = false }()
		if eventName == "error" {
			result.Events++
			return errProbeUpstream
		}
		if !hasData {
			return nil
		}
		var event warpapi.ResponseEvent
		encoded := frame.String()
		encoding := base64.RawURLEncoding.Strict()
		if strings.HasSuffix(encoded, "=") {
			encoding = base64.URLEncoding.Strict()
		}
		data, err := encoding.DecodeString(encoded)
		if err != nil || len(data) == 0 || proto.Unmarshal(data, &event) != nil || !event.HasType() {
			return errProbeStream
		}
		result.Events++
		switch event.WhichType() {
		case warpapi.ResponseEvent_Init_case:
			return nil
		case warpapi.ResponseEvent_Finished_case:
			usage, err := extractProbeUsage(event.GetFinished())
			if err != nil {
				return err
			}
			result.Usage = usage
			if txDepth != 0 {
				return errProbeIncomplete
			}
			if event.GetFinished().WhichReason() != warpapi.ResponseEvent_StreamFinished_Done_case {
				return errProbeUpstream
			}
			result.Complete = true
		case warpapi.ResponseEvent_ClientActions_case:
			for _, action := range event.GetClientActions().GetActions() {
				var messages []*warpapi.Message
				appendOnly := false
				switch action.WhichAction() {
				case warpapi.ClientAction_CreateTask_case:
					messages = action.GetCreateTask().GetTask().GetMessages()
				case warpapi.ClientAction_AddMessagesToTask_case:
					messages = action.GetAddMessagesToTask().GetMessages()
				case warpapi.ClientAction_UpdateTaskMessage_case:
					messages = []*warpapi.Message{action.GetUpdateTaskMessage().GetMessage()}
				case warpapi.ClientAction_AppendToMessageContent_case:
					messages = []*warpapi.Message{action.GetAppendToMessageContent().GetMessage()}
					appendOnly = true
				case warpapi.ClientAction_BeginTransaction_case:
					// Snapshot counters so rolled-back output never counts as success.
					if txDepth != 0 {
						return errProbeStream
					}
					txDepth++
					if txDepth == 1 {
						txText = result.TextBytes
						txLengths = make(map[string]int, len(textLengths))
						for k, v := range textLengths {
							txLengths[k] = v
						}
					}
				case warpapi.ClientAction_CommitTransaction_case:
					if txDepth != 1 {
						return errProbeStream
					}
					if txDepth > 0 {
						txDepth--
					}
				case warpapi.ClientAction_RollbackTransaction_case:
					if txDepth != 1 {
						return errProbeStream
					}
					if txDepth > 0 {
						result.TextBytes = txText
						textLengths = txLengths
						txDepth = 0
					}
				case warpapi.ClientAction_MoveMessagesToNewTask_case:
					// This probe has no multi-task/history migration semantics.
					return errProbeStream
				case warpapi.ClientAction_StartNewConversation_case, warpapi.ClientAction_UpdateTaskServerData_case:
					return errProbeStream
				case warpapi.ClientAction_ShowSuggestions_case, warpapi.ClientAction_UpdateTaskSummary_case, warpapi.ClientAction_UpdateTaskDescription_case:
					// Non-executable bookkeeping has no answer text.
				default:
					return fmt.Errorf("%w: client_action case=%d", errProbeStream, int(action.WhichAction()))
				}
				for _, message := range messages {
					switch message.WhichMessage() {
					case warpapi.Message_ToolCall_case:
						// Server-side tools also cost credits. No tool type is exempt.
						result.ToolRequested = true
						return errProbeTool
					case warpapi.Message_WebSearch_case, warpapi.Message_WebFetch_case, warpapi.Message_InvokeSkill_case:
						result.ToolRequested = true
						return fmt.Errorf("%w: message case=%d", errProbeTool, int(message.WhichMessage()))
					case warpapi.Message_ServerEvent_case, warpapi.Message_ToolCallResult_case, warpapi.Message_MessagesReceivedFromAgents_case, warpapi.Message_EventsFromAgents_case, warpapi.Message_OrchestrationConfigSnapshot_case:
						result.ToolRequested = true
						return errProbeTool
					case warpapi.Message_ModelUsed_case:
						used := message.GetModelUsed()
						if used.GetIsFallback() || (expectedModel != "" && used.GetModelId() != expectedModel) {
							return errors.New("upstream model substitution rejected")
						}
					case warpapi.Message_RequestMetadata_case:
						meta := message.GetRequestMetadata()
						if meta.GetIncomplete() || meta.GetOutcome() == warpapi.Message_RequestMetadata_OUTCOME_ERRORED || meta.GetOutcome() == warpapi.Message_RequestMetadata_OUTCOME_CANCELED {
							return errProbeIncomplete
						}
						summary := meta.GetToolCallSummary()
						if summary.GetToolCalls() > 0 || summary.GetCommandsExecuted() > 0 || summary.GetFilesChanged() > 0 || summary.GetLinesAdded() > 0 || summary.GetLinesRemoved() > 0 {
							result.ToolRequested = true
							return errProbeTool
						}
					case warpapi.Message_AgentOutput_case:
						id := message.GetId()
						if id == "" {
							return errProbeStream
						}
						n := len(message.GetAgentOutput().GetText())
						if appendOnly {
							result.TextBytes += n
							textLengths[id] += n
						} else {
							result.TextBytes += n - textLengths[id]
							textLengths[id] = n
						}
						if result.TextBytes > 4096 {
							return errProbeLimit
						}
					case warpapi.Message_UserQuery_case, warpapi.Message_AgentReasoning_case:
						// Echoed fixed input and reasoning are never counted as output.
					default:
						return errProbeStream
					}
				}
			}
		}
		return nil
	}
	for {
		var line []byte
		for {
			fragment, err := reader.ReadSlice('\n')
			if limited.N == 0 {
				return result, errProbeLimit
			}
			if len(line)+len(fragment) > probeFrameLimit {
				return result, errProbeLimit
			}
			line = append(line, fragment...)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil && err != io.EOF {
				return result, errProbeStream
			}
			if len(line) == 0 && err == io.EOF {
				if flushErr := flush(); flushErr != nil {
					return result, flushErr
				}
				if result.Complete {
					return result, nil
				}
				return result, errProbeIncomplete
			}
			break
		}
		frameBytes += len(line)
		if frameBytes > probeFrameLimit {
			return result, errProbeLimit
		}
		text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		if text == "" {
			if err := flush(); err != nil {
				return result, err
			}
			if result.Complete {
				return result, nil
			}
			continue
		}
		if strings.HasPrefix(text, ":") {
			continue
		}
		field, value, _ := strings.Cut(text, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			eventName = value
		case "data":
			// Warp transports one base64url value, sometimes split over data
			// lines; concatenate those fragments before protobuf decoding.
			frame.WriteString(value)
			hasData = true
		}
	}
}
