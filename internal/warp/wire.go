package warp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
)

const (
	frameLimit  = 1 << 20
	streamLimit = 8 << 20
)

var (
	errStreamStream     = errors.New("invalid warp stream")
	errStreamLimit      = errors.New("warp stream size limit exceeded")
	errStreamUpstream   = errors.New("warp upstream error")
	errStreamTool       = errors.New("warp requested a tool")
	errStreamIncomplete = errors.New("warp stream ended without completion")
)

type streamResult struct {
	TextBytes     int
	Complete      bool
	ToolRequested bool
	// ServerBookkeeping counts opaque variants the official client renders as
	// no client action. It is not proof that no server-side work occurred.
	ServerBookkeeping int
	Events            int
	// Rejection identifiers are enum numbers only (never payloads), so a future
	// failure names the exact protocol feature without capturing content.
	RejectAction  int
	RejectMessage int
	RejectTool    int
	Rejection     *streamRejection
	Usage         observedUsage
	Text          string
	ToolCalls     []toolCall
	// Upstream conversation identity from the Init event. A follow-up request
	// reuses it so the server can correlate a tool result with the call it
	// answers instead of starting an unrelated conversation.
	ConversationID      string
	RequestID           string
	RunID               string
	RootTaskContext     []byte
	RecoveryContext     []byte
	RecoverySafe        bool
	Failure             *streamFailure
	ModelMismatch       *observedModelMismatch
	ModelUsed           *observedModelMismatch
	FinishReason        string
	ConversationContext *conversationContextDiagnostic
}

// toolCall is one client-executable MCP tool invocation observed upstream.
type toolCall struct {
	ID   string
	Name string
	Args string
}

// buildRequest never reads the environment, filesystem, clock, or history.
func buildRequest(model string, prepared preparedRequest) ([]byte, error) {
	if !validModel(model) || strings.TrimSpace(model) != model || len(model) > 256 || !utf8.ValidString(model) || strings.ContainsAny(model, "\r\n\x00") {
		return nil, errors.New("invalid warp model")
	}
	no, yes := false, true
	empty := ""
	platform, shell := "Linux", "sh"
	autonomy, isolation := warpapi.AutonomyLevel_SUPERVISED, warpapi.IsolationLevel_SANDBOX
	modelConfig := warpapi.Request_Settings_ModelConfig_builder{Base: &model}
	if prepared.ContextWindow > 0 {
		window := uint32(prepared.ContextWindow)
		modelConfig.BaseModelContextWindowLimit = &window
	}
	// With client tools the server may only emit MCP tool calls, which the client
	// executes. Without them the fence advertises no executable tool at all.
	supportedTools := []warpapi.ToolType{warpapi.ToolType_SUGGEST_PROMPT}
	parallel := false
	if len(prepared.Tools) > 0 {
		supportedTools = []warpapi.ToolType{warpapi.ToolType_CALL_MCP_TOOL}
		parallel = prepared.ParallelTools
	}
	settings := warpapi.Request_Settings_builder{
		ModelConfig:                 modelConfig.Build(),
		RulesEnabled:                &no,
		WebContextRetrievalEnabled:  &no,
		SupportsParallelToolCalls:   &parallel,
		UseAnthropicTextEditorTools: &no,
		PlanningEnabled:             &no,
		WarpDriveContextEnabled:     &no,
		SupportsCreateFiles:         &no,
		// An empty list permits ANY tool. This passive fence advertises no
		// executable tool; suggestions are also disabled explicitly below.
		SupportedTools:                             supportedTools,
		SupportedCliAgentTools:                     []warpapi.ToolType{warpapi.ToolType_SUGGEST_PROMPT},
		SupportsLongRunningCommands:                &no,
		ShouldPreserveFileContentInHistory:         &yes,
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
		SupportsChatgptSubscriptionError:           &no,
		SupportsServerSideAgentMessageInjection:    &no,
		SupportsChatgptSubscriptionCopyText:        &no,
	}.Build()
	agent := warpapi.AgentType_AGENT_TYPE_PRIMARY
	inputs := make([]*warpapi.Request_Input_UserInputs_UserInput, 0, len(prepared.ToolResults)+1)
	for _, result := range prepared.ToolResults {
		inputs = append(inputs, warpapi.Request_Input_UserInputs_UserInput_builder{
			ToolCallResult: buildMCPToolResult(result),
		}.Build())
	}
	if prepared.Query != "" {
		inputs = append(inputs, warpapi.Request_Input_UserInputs_UserInput_builder{
			UserQuery: warpapi.Request_Input_UserQuery_builder{Query: &prepared.Query, Mode: warpapi.UserQueryMode_builder{}.Build(), IntendedAgent: &agent}.Build(),
		}.Build())
	}
	taskContext := warpapi.Request_TaskContext_builder{}.Build()
	serverID := clientMCPServerID
	if len(prepared.RootTaskContext) > 0 {
		var err error
		taskContext, err = decodeTaskContext(prepared.RootTaskContext)
		if err != nil {
			return nil, err
		}
		serverID, err = mcpServerIDForRoot(taskContext.GetTasks()[0])
		if err != nil {
			return nil, err
		}
	}
	request := warpapi.Request_builder{
		TaskContext: taskContext,
		Input: warpapi.Request_Input_builder{
			Context: warpapi.InputContext_builder{
				Directory:       warpapi.InputContext_Directory_builder{Pwd: &empty, Home: &empty, PwdFileSymbolsIndexed: &no}.Build(),
				OperatingSystem: warpapi.InputContext_OperatingSystem_builder{Platform: &platform, Distribution: &empty}.Build(),
				Shell:           warpapi.InputContext_Shell_builder{Name: &shell, Version: &empty}.Build(),
			}.Build(),
			UserInputs: warpapi.Request_Input_UserInputs_builder{Inputs: inputs}.Build(),
		}.Build(),
		Settings:   settings,
		Metadata:   buildMetadata(prepared.ConversationID),
		McpContext: buildMCPContext(prepared.Tools, serverID),
	}.Build()
	if prepared.Resume {
		if prepared.ConversationID == "" || len(prepared.RootTaskContext) == 0 {
			return nil, errContinuationState
		}
		request.GetInput().SetResumeConversation(warpapi.Request_Input_ResumeConversation_builder{}.Build())
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(request)
}

// buildMetadata continues an existing upstream conversation when a tool result
// answers a call issued in it. The server correlates the call id through this id.
func buildMetadata(conversationID string) *warpapi.Request_Metadata {
	builder := warpapi.Request_Metadata_builder{}
	if conversationID != "" {
		builder.ConversationId = &conversationID
	}
	return builder.Build()
}

// buildMCPToolResult encodes one client tool result as a typed MCP result.
func buildMCPToolResult(result preparedToolResult) *warpapi.Request_Input_ToolCallResult {
	id := result.ID
	builder := warpapi.Request_Input_ToolCallResult_builder{ToolCallId: &id}
	if result.IsError {
		builder.CallMcpTool = warpapi.CallMCPToolResult_builder{
			Error: warpapi.CallMCPToolResult_Error_builder{Message: proto.String(result.Content)}.Build(),
		}.Build()
		return builder.Build()
	}
	text := result.Content
	builder.CallMcpTool = warpapi.CallMCPToolResult_builder{
		Success: warpapi.CallMCPToolResult_Success_builder{Results: []*warpapi.CallMCPToolResult_Success_Result{
			warpapi.CallMCPToolResult_Success_Result_builder{
				Text: warpapi.CallMCPToolResult_Success_Result_Text_builder{Text: &text}.Build(),
			}.Build(),
		}}.Build(),
	}.Build()
	return builder.Build()
}

func readStreamWithProgress(source io.Reader, expectedModel string, initial []byte, progress func()) (result streamResult, parseErr error) {
	state := newContinuationAccumulator(initial)
	models := newModelObservations(state.root)
	serverID, serverErr := mcpServerIDForRoot(state.root)
	if state.err != nil || serverErr != nil {
		return result, errStreamStream
	}
	recoveryCandidate := false
	checkpointProgress := false
	defer func() {
		if parseErr != nil && result.Failure == nil {
			result.Failure = failureFromState("stream", state)
		}
		if parseErr != nil && recoveryCandidate && checkpointProgress && result.Rejection == nil && state.err == nil && result.ConversationID != "" {
			if snapshot, e := state.snapshot(); e == nil {
				result.RecoveryContext = snapshot
				result.RecoverySafe = true
			}
		}
	}()
	if source == nil {
		return result, errStreamStream
	}
	limited := &io.LimitedReader{R: source, N: streamLimit + 1}
	reader := bufio.NewReaderSize(limited, 32<<10)
	var frame strings.Builder
	var eventName string
	frameBytes := 0
	hasData := false
	// Message snapshots replace prior content; append actions append exactly once.
	texts := make(map[string]string)
	var order []string
	txDepth, txText := 0, 0
	var txTexts map[string]string
	var txOrder []string
	// Tool identity is the upstream call ID, not the enclosing message ID:
	// Add/Update snapshots may replay the same invocation more than once.
	toolCalls := make(map[string]toolCall)
	var txToolCalls []toolCall
	var txMismatch *observedModelMismatch
	var taskID string
	createdTask := false
	previousMCP := make(map[string]bool)
	serverCalls := observedServerCalls{calls: make(map[string]bool)}
	if root := state.root; root != nil {
		taskID, createdTask = root.GetId(), true
		for _, message := range root.GetMessages() {
			serverCalls.observe(message, taskID)
			call := message.GetToolCall()
			if call != nil && call.WhichTool() == warpapi.Message_ToolCall_CallMcpTool_case && call.GetToolCallId() != "" {
				previousMCP[call.GetToolCallId()] = true
			}
		}
	}
	bindTask := func(id string) bool {
		if id == "" {
			return true
		}
		if taskID == "" {
			taskID = id
			return true
		}
		return taskID == id
	}
	flush := func() error {
		defer func() { frame.Reset(); frameBytes = 0; eventName = ""; hasData = false }()
		if eventName == "error" {
			result.Events++
			return errStreamUpstream
		}
		// Like the official decoder, only the error label has special
		// meaning. Every other bounded label carries the same protobuf data;
		// frames without data are keepalives, regardless of their label.
		if !hasData {
			return nil
		}
		var event warpapi.ResponseEvent
		// Match the official Rust decoder's trim_matches('"') contract:
		// accept raw base64url or its quoted form, not arbitrary JSON.
		encoded := strings.Trim(frame.String(), "\"")
		encoding := base64.RawURLEncoding.Strict()
		if strings.HasSuffix(encoded, "=") {
			encoding = base64.URLEncoding.Strict()
		}
		data, err := encoding.DecodeString(encoded)
		if err != nil || len(data) == 0 || proto.Unmarshal(data, &event) != nil || !event.HasType() || hasUnknown(event.ProtoReflect()) {
			return errStreamStream
		}
		result.Events++
		switch event.WhichType() {
		case warpapi.ResponseEvent_Init_case:
			init := event.GetInit()
			result.ConversationID = init.GetConversationId()
			result.RequestID = init.GetRequestId()
			result.RunID = init.GetRunId()
			return nil
		case warpapi.ResponseEvent_Finished_case:
			result.FinishReason = observedFinishReason(event.GetFinished())
			result.ConversationContext = observeConversationContext(event.GetFinished().GetConversationUsageMetadata())
			usage, err := extractUsage(event.GetFinished())
			if err != nil {
				return err
			}
			// A finish without measurements must not erase request charges already
			// observed in metadata, including on a rejected terminal reason.
			if usage.Measured || !result.Usage.Measured {
				result.Usage = usage
			}
			if usageHasExternalTools(event.GetFinished()) {
				result.ToolRequested = true
				return errStreamTool
			}
			if txDepth != 0 {
				return errStreamIncomplete
			}
			if event.GetFinished().WhichReason() != warpapi.ResponseEvent_StreamFinished_Done_case {
				reason := event.GetFinished().WhichReason()
				recoveryCandidate = reason == warpapi.ResponseEvent_StreamFinished_InternalError_case || reason == warpapi.ResponseEvent_StreamFinished_LlmUnavailable_case
				result.Failure = &streamFailure{Site: "finish", Reason: "non_done", EventOrdinal: result.Events, ReasonKind: int(reason)}
				return errStreamUpstream
			}
			if result.TextBytes == 0 && len(result.ToolCalls) == 0 {
				return errStreamIncomplete
			}
			for _, id := range order {
				result.Text += texts[id]
			}
			if len(result.ToolCalls) > 0 {
				var err error
				result.RootTaskContext, err = state.snapshot()
				if err != nil {
					result.Failure = failureFromState("snapshot", state)
					return errStreamStream
				}
			}
			result.Complete = true
		case warpapi.ResponseEvent_ClientActions_case:
			for actionIndex, action := range event.GetClientActions().GetActions() {
				var messages []*warpapi.Message
				appendOnly := false
				switch action.WhichAction() {
				case warpapi.ClientAction_CreateTask_case:
					if createdTask {
						return errStreamTool
					}
					createdTask = true
					task := action.GetCreateTask().GetTask()
					if task.GetDependencies().GetParentTaskId() != "" || !bindTask(task.GetId()) {
						return errStreamTool
					}
					messages = task.GetMessages()
				case warpapi.ClientAction_AddMessagesToTask_case:
					if !bindTask(action.GetAddMessagesToTask().GetTaskId()) {
						result.Rejection = rejectionAt("task_mismatch", "action_task", "none", taskID, result.Events, actionIndex+1, 0, action, nil)
						return errStreamTool
					}
					messages = action.GetAddMessagesToTask().GetMessages()
				case warpapi.ClientAction_UpdateTaskMessage_case:
					if !bindTask(action.GetUpdateTaskMessage().GetTaskId()) {
						result.Rejection = rejectionAt("task_mismatch", "action_task", "none", taskID, result.Events, actionIndex+1, 0, action, nil)
						return errStreamTool
					}
					messages = []*warpapi.Message{action.GetUpdateTaskMessage().GetMessage()}
				case warpapi.ClientAction_AppendToMessageContent_case:
					if !bindTask(action.GetAppendToMessageContent().GetTaskId()) {
						return errStreamTool
					}
					messages = []*warpapi.Message{action.GetAppendToMessageContent().GetMessage()}
					appendOnly = true
				case warpapi.ClientAction_BeginTransaction_case:
					// Rollbacks restore content, message order, and executable calls.
					if txDepth != 0 {
						return errStreamStream
					}
					serverCalls.begin(taskID, createdTask, result.ServerBookkeeping)
					models.begin()
					txMismatch = result.ModelMismatch
					txDepth++
					if txDepth == 1 {
						txText = result.TextBytes
						txTexts = make(map[string]string, len(texts))
						txOrder = append([]string(nil), order...)
						txToolCalls = append([]toolCall(nil), result.ToolCalls...)
						for k, v := range texts {
							txTexts[k] = v
						}
					}
				case warpapi.ClientAction_CommitTransaction_case:
					if txDepth != 1 {
						return errStreamStream
					}
					if txDepth > 0 {
						txDepth--
					}
					models.commit()
				case warpapi.ClientAction_RollbackTransaction_case:
					if txDepth != 1 {
						return errStreamStream
					}
					if txDepth > 0 {
						models.rollback()
						result.ModelMismatch = txMismatch
						taskID, createdTask, result.ServerBookkeeping = serverCalls.rollback()
						result.TextBytes = txText
						texts = txTexts
						order = txOrder
						result.ToolCalls = txToolCalls
						toolCalls = make(map[string]toolCall, len(txToolCalls))
						for _, call := range txToolCalls {
							toolCalls[call.ID] = call
						}
						txDepth = 0
					}
				case warpapi.ClientAction_MoveMessagesToNewTask_case:
					// This channel has no multi-task/history migration semantics.
					result.RejectAction = int(action.WhichAction())
					return errStreamStream
				case warpapi.ClientAction_UpdateTaskServerData_case:
					// Opaque bookkeeping may only reference the already bound
					// root task. Retain it only in private continuation state;
					// never interpret it or return it as client-visible output.
					if !createdTask || action.GetUpdateTaskServerData().GetTaskId() != taskID {
						result.RejectAction = int(action.WhichAction())
						return errStreamTool
					}
				case warpapi.ClientAction_StartNewConversation_case:
					result.RejectAction = int(action.WhichAction())
					return errStreamStream
				case warpapi.ClientAction_ShowSuggestions_case, warpapi.ClientAction_UpdateTaskSummary_case, warpapi.ClientAction_UpdateTaskDescription_case:
					// Non-executable bookkeeping has no answer text.
				default:
					result.RejectAction = int(action.WhichAction())
					return errStreamStream
				}
				preview := messageActionPreview{state: state, action: action}
				for messageIndex, message := range messages {
					if !bindTask(message.GetTaskId()) {
						result.RejectMessage = int(message.WhichMessage())
						result.Rejection = rejectionAt("task_mismatch", "message_task", "none", taskID, result.Events, actionIndex+1, messageIndex+1, action, message)
						return errStreamTool
					}
					var err error
					effectiveText := false
					message, effectiveText, err = preview.visible(message)
					if err != nil {
						result.Failure = failureFromState("masked_update", state)
						return errStreamStream
					}
					message, err = preview.bookkeeping(message)
					if err != nil {
						return err
					}
					models.observe(message, result.Events, actionIndex+1, messageIndex+1)
					switch message.WhichMessage() {
					case warpapi.Message_ToolCall_case:
						// Tool::Server is opaque server-resolved bookkeeping with no
						// client action. Only validated client MCP calls may execute;
						// shell, files, subagents, and skills stay rejected.
						if isServerBookkeeping(message) {
							if createdTask {
								serverCalls.observe(message, taskID)
							}
							result.ServerBookkeeping++
							continue
						}
						call := message.GetToolCall()
						if call.WhichTool() == warpapi.Message_ToolCall_CallMcpTool_case {
							invoke := call.GetCallMcpTool()
							// encoding/json sorts object keys, making replay comparison
							// independent of protobuf map iteration order.
							args, err := json.Marshal(invoke.GetArgs().AsMap())
							candidate := toolCall{ID: call.GetToolCallId(), Name: invoke.GetName(), Args: string(args)}
							prior, exists := toolCalls[candidate.ID]
							if err != nil || candidate.ID == "" || candidate.Name == "" ||
								(invoke.HasServerId() && invoke.GetServerId() != serverID) ||
								(exists && prior != candidate) {
								result.ToolRequested = true
								result.RejectMessage = int(message.WhichMessage())
								result.RejectTool = int(call.WhichTool())
								return errStreamTool
							}
							if !exists {
								toolCalls[candidate.ID] = candidate
								result.ToolCalls = append(result.ToolCalls, candidate)
							}
							continue
						}
						result.ToolRequested = true
						result.RejectMessage = int(message.WhichMessage())
						result.RejectTool = int(call.WhichTool())
						return errStreamTool
					case warpapi.Message_ServerEvent_case:
						// Official history events are opaque server-side data, not a
						// client action. Payload is never interpreted or returned.
						if isServerBookkeeping(message) {
							result.ServerBookkeeping++
							continue
						}
						result.ToolRequested = true
						result.RejectMessage = int(message.WhichMessage())
						return errStreamTool
					case warpapi.Message_ToolCallResult_case:
						// Server results remain opaque and require a previously observed
						// same-root Server call. Only prior client MCP results may echo.
						if err := rejectToolResult(&result, taskID, actionIndex+1, messageIndex+1, action, message, previousMCP, serverCalls.calls); err != nil {
							return err
						}
						if message.GetToolCallResult().WhichResult() == warpapi.Message_ToolCallResult_Server_case {
							if err := preview.evaluate(); err != nil {
								return err
							}
						}
					case warpapi.Message_WebSearch_case, warpapi.Message_WebFetch_case, warpapi.Message_InvokeSkill_case, warpapi.Message_MessagesReceivedFromAgents_case, warpapi.Message_EventsFromAgents_case, warpapi.Message_OrchestrationConfigSnapshot_case:
						result.ToolRequested = true
						result.RejectMessage = int(message.WhichMessage())
						return errStreamTool
					case warpapi.Message_ModelUsed_case:
						if models.inherited[message.GetId()] {
							continue
						}
						used := message.GetModelUsed()
						// Model choice is advisory: accept upstream fallback while
						// retaining the first discrepancy for private diagnostics.
						if result.ModelMismatch == nil && (used.GetIsFallback() || (expectedModel != "" && used.GetModelId() != expectedModel)) {
							result.ModelMismatch = &observedModelMismatch{ID: used.GetModelId(), Fallback: used.GetIsFallback(), Event: result.Events, Action: actionIndex + 1, Message: messageIndex + 1}
						}
					case warpapi.Message_RequestMetadata_case:
						meta := message.GetRequestMetadata()
						if meta.HasCharges() {
							finish := warpapi.ResponseEvent_StreamFinished_builder{RequestCharges: meta.GetCharges()}.Build()
							usage, err := extractUsage(finish)
							if err != nil {
								return err
							}
							result.Usage = usage
							if usageHasExternalTools(finish) {
								result.ToolRequested = true
								return errStreamTool
							}
						}
						if meta.GetIncomplete() || meta.GetOutcome() == warpapi.Message_RequestMetadata_OUTCOME_ERRORED || meta.GetOutcome() == warpapi.Message_RequestMetadata_OUTCOME_CANCELED {
							return errStreamIncomplete
						}
						summary := meta.GetToolCallSummary()
						// Counts may summarize the unique MCP invocations validated in
						// this stream, never unobserved or rolled-back executable work.
						if uint64(summary.GetToolCalls()) > uint64(len(result.ToolCalls)) || summary.GetCommandsExecuted() > 0 || summary.GetFilesChanged() > 0 || summary.GetLinesAdded() > 0 || summary.GetLinesRemoved() > 0 {
							result.ToolRequested = true
							return errStreamTool
						}
					case warpapi.Message_AgentOutput_case:
						id := message.GetId()
						if id == "" {
							return errStreamStream
						}
						text := message.GetAgentOutput().GetText()
						if !utf8.ValidString(text) {
							return errStreamStream
						}
						if _, exists := texts[id]; !exists {
							order = append(order, id)
						}
						n := len(text)
						if appendOnly && !effectiveText {
							result.TextBytes += n
							texts[id] += text
						} else {
							result.TextBytes += n - len(texts[id])
							texts[id] = text
						}
					case warpapi.Message_UserQuery_case, warpapi.Message_AgentReasoning_case:
						// Echoed user input and reasoning are never counted as output.
					default:
						result.RejectMessage = int(message.WhichMessage())
						return errStreamStream
					}
				}
				result.ModelUsed = models.latest()
				// Track the complete validated root state privately for a client
				// tool follow-up. Pure text fixtures may not contain CreateTask;
				// only a tool-bearing completion requires a valid snapshot.
				if err := preview.commit(); err == nil {
					switch action.WhichAction() {
					case warpapi.ClientAction_AddMessagesToTask_case, warpapi.ClientAction_UpdateTaskMessage_case, warpapi.ClientAction_AppendToMessageContent_case, warpapi.ClientAction_UpdateTaskServerData_case:
						checkpointProgress = true
					}
				} else if result.Failure == nil {
					result.Failure = failureFromState("state_apply", state)
					result.Failure.EventOrdinal = result.Events
					result.Failure.ActionOrdinal = actionIndex + 1
					result.Failure.ActionKind = int(action.WhichAction())
				}
			}
		default:
			return errStreamStream
		}
		if progress != nil && len(event.GetClientActions().GetActions()) > 0 {
			progress()
		}
		return nil
	}
	firstLine := true
	for {
		var line []byte
		for {
			fragment, err := reader.ReadSlice('\n')
			if limited.N == 0 {
				return result, errStreamLimit
			}
			if len(line)+len(fragment) > frameLimit {
				return result, errStreamLimit
			}
			line = append(line, fragment...)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil && err != io.EOF {
				recoveryCandidate = true
				result.Failure = &streamFailure{Site: "transport_read", Reason: "read_failed", EventOrdinal: result.Events}
				if isGenerationTimeout(err) {
					result.Failure.Reason = generationTimeoutReason(err)
					return result, err
				}
				if errors.Is(err, context.Canceled) {
					result.Failure.Reason = "canceled"
					return result, context.Canceled
				}
				return result, errStreamStream
			}
			if len(line) == 0 && err == io.EOF {
				if flushErr := flush(); flushErr != nil {
					return result, flushErr
				}
				if result.Complete {
					return result, nil
				}
				recoveryCandidate = true
				result.Failure = &streamFailure{Site: "transport_read", Reason: "eof_before_finish", EventOrdinal: result.Events}
				return result, errStreamIncomplete
			}
			break
		}
		frameBytes += len(line)
		if frameBytes > frameLimit {
			return result, errStreamLimit
		}
		text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		if firstLine {
			// SSE permits a single UTF-8 BOM at the start of the stream.
			text = strings.TrimPrefix(text, "\ufeff")
			firstLine = false
		}
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
