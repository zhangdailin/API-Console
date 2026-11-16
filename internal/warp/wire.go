package warp

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	outputLimit = 16 << 10
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
	Usage         observedUsage
	Text          string
	ToolCalls     []toolCall
	// Upstream conversation identity from the Init event. A follow-up request
	// reuses it so the server can correlate a tool result with the call it
	// answers instead of starting an unrelated conversation.
	ConversationID  string
	RequestID       string
	RunID           string
	RootTaskContext []byte
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
	no := false
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
	if len(prepared.RootTaskContext) > 0 {
		if _, err := decodeContinuationState(prepared.RootTaskContext); err != nil {
			return nil, err
		}
		if err := proto.Unmarshal(prepared.RootTaskContext, taskContext); err != nil {
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
		McpContext: buildMCPContext(prepared.Tools),
	}.Build()
	return proto.MarshalOptions{Deterministic: true}.Marshal(request)
}

// buildMCPContext declares the client-executable tools. The server may only emit
// a tool call from this list, so nothing shell- or file-capable is advertised.
func buildMCPContext(tools []mcpTool) *warpapi.Request_MCPContext {
	if len(tools) == 0 {
		return nil
	}
	declared := make([]*warpapi.Request_MCPContext_MCPTool, 0, len(tools))
	for _, tool := range tools {
		builder := warpapi.Request_MCPContext_MCPTool_builder{Name: &tool.Name}
		if tool.Description != "" {
			builder.Description = &tool.Description
		}
		// An empty JSON object is still an explicitly supplied valid schema.
		// The official converter always sets input_schema after conversion.
		if tool.Schema != nil {
			if schema, err := structpb.NewStruct(tool.Schema); err == nil {
				builder.InputSchema = schema
			}
		}
		declared = append(declared, builder.Build())
	}
	server := warpapi.Request_MCPContext_MCPServer_builder{
		Name:        proto.String("client"),
		Description: proto.String("Tools declared by the client request"),
		Id:          proto.String("client-request-tools"),
		Tools:       declared,
	}.Build()
	return warpapi.Request_MCPContext_builder{Servers: []*warpapi.Request_MCPContext_MCPServer{server}}.Build()
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

// readStream buffers output until a safe terminal event.
func readStream(source io.Reader, expectedModel string) (streamResult, error) {
	return readStreamWithContext(source, expectedModel, nil)
}

func readStreamWithContext(source io.Reader, expectedModel string, initial []byte) (streamResult, error) {
	var result streamResult
	state := newContinuationAccumulator(initial)
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
	var taskID string
	createdTask := false
	previousMCP := make(map[string]bool)
	if len(initial) > 0 {
		root, err := decodeContinuationState(initial)
		if err != nil {
			return result, errStreamStream
		}
		taskID, createdTask = root.GetId(), true
		for _, message := range root.GetMessages() {
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
					return errStreamStream
				}
			}
			result.Complete = true
		case warpapi.ResponseEvent_ClientActions_case:
			for _, action := range event.GetClientActions().GetActions() {
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
						return errStreamTool
					}
					messages = action.GetAddMessagesToTask().GetMessages()
				case warpapi.ClientAction_UpdateTaskMessage_case:
					if !bindTask(action.GetUpdateTaskMessage().GetTaskId()) {
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
				case warpapi.ClientAction_RollbackTransaction_case:
					if txDepth != 1 {
						return errStreamStream
					}
					if txDepth > 0 {
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
					// root task. Never retain, interpret, or return server_data.
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
				for _, message := range messages {
					if !bindTask(message.GetTaskId()) {
						result.RejectMessage = int(message.WhichMessage())
						return errStreamTool
					}
					switch message.WhichMessage() {
					case warpapi.Message_ToolCall_case:
						// Tool::Server is opaque server-resolved bookkeeping with no
						// client action. Only validated client MCP calls may execute;
						// shell, files, subagents, and skills stay rejected.
						if isServerBookkeeping(message) {
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
								(invoke.HasServerId() && invoke.GetServerId() != "client-request-tools") ||
								(exists && prior != candidate) || (!exists && len(result.ToolCalls) >= 6) {
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
						// A resumed task echoes the exact client MCP result it was
						// supplied. Unbound/native/server results still fail closed.
						resultMessage := message.GetToolCallResult()
						if resultMessage.WhichResult() != warpapi.Message_ToolCallResult_CallMcpTool_case || !previousMCP[resultMessage.GetToolCallId()] {
							result.ToolRequested = true
							result.RejectMessage = int(message.WhichMessage())
							return errStreamTool
						}
					case warpapi.Message_WebSearch_case, warpapi.Message_WebFetch_case, warpapi.Message_InvokeSkill_case, warpapi.Message_MessagesReceivedFromAgents_case, warpapi.Message_EventsFromAgents_case, warpapi.Message_OrchestrationConfigSnapshot_case:
						result.ToolRequested = true
						result.RejectMessage = int(message.WhichMessage())
						return errStreamTool
					case warpapi.Message_ModelUsed_case:
						used := message.GetModelUsed()
						if used.GetIsFallback() || (expectedModel != "" && used.GetModelId() != expectedModel) {
							return errStreamModelSubstitution
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
						if appendOnly {
							result.TextBytes += n
							texts[id] += text
						} else {
							result.TextBytes += n - len(texts[id])
							texts[id] = text
						}
						if result.TextBytes > outputLimit {
							return errStreamLimit
						}
					case warpapi.Message_UserQuery_case, warpapi.Message_AgentReasoning_case:
						// Echoed user input and reasoning are never counted as output.
					default:
						result.RejectMessage = int(message.WhichMessage())
						return errStreamStream
					}
				}
				// Track the complete validated root state privately for a client
				// tool follow-up. Pure text fixtures may not contain CreateTask;
				// only a tool-bearing completion requires a valid snapshot.
				_ = state.apply(action)
			}
		default:
			return errStreamStream
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
				return result, errStreamStream
			}
			if len(line) == 0 && err == io.EOF {
				if flushErr := flush(); flushErr != nil {
					return result, flushErr
				}
				if result.Complete {
					return result, nil
				}
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

// New schema fields may introduce execution semantics; fail closed until reviewed.
func hasUnknown(message protoreflect.Message) bool {
	if len(message.GetUnknown()) != 0 {
		return true
	}
	unknown := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.IsMap() {
			if field.MapValue().Kind() == protoreflect.MessageKind {
				value.Map().Range(func(_ protoreflect.MapKey, v protoreflect.Value) bool {
					unknown = hasUnknown(v.Message())
					return !unknown
				})
			}
		} else if field.IsList() {
			if field.Kind() == protoreflect.MessageKind {
				list := value.List()
				for i := 0; i < list.Len(); i++ {
					if hasUnknown(list.Get(i).Message()) {
						unknown = true
						break
					}
				}
			}
		} else if field.Kind() == protoreflect.MessageKind {
			unknown = hasUnknown(value.Message())
		}
		return !unknown
	})
	return unknown
}
