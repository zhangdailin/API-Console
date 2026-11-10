package responses

import (
	"encoding/json"
	"fmt"
	"orchids-api/internal/chatwire"
	"strings"
)

// Codex declares tools a function-only upstream cannot accept: freeform
// `custom` tools, the structured `apply_patch` tool, and `tool_search` with
// deferred (`defer_loading`) functions. A client-executed tool_search is a
// function the client answers with tool definitions; those are declared to
// the upstream from the replayed tool_search_output on the next call. The client executes all of them, so
// each is lowered to an ordinary function with a fixed argument shape and the
// call is raised back to the client's item type on the way out. The identity
// rides in ToolNamespaces, so it is persisted and restored by the same
// plumbing as namespace aliases.
const (
	toolKindCustom     = "custom"
	toolKindApplyPatch = "apply_patch"
	applyPatchWireName = "apply_patch"
	// toolKindToolSearch is a client-executed tool_search: the model asks the
	// client (Codex) to search its deferred tools, and the client returns
	// their definitions in a tool_search_output for the next call.
	toolKindToolSearch = "tool_search"
	toolSearchWireName = "tool_search"
	// maxGrammarDescription bounds the grammar text copied into a lowered
	// custom tool's description.
	maxGrammarDescription = 8 << 10
)

// applyPatchParameters is the V4A operation shape of an apply_patch_call.
var applyPatchParameters = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"operation": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"type": map[string]any{"type": "string", "enum": []any{"create_file", "update_file", "delete_file"}},
				"path": map[string]any{"type": "string"},
				"diff": map[string]any{"type": "string"},
			},
			"required":             []any{"type", "path"},
			"additionalProperties": false,
		},
	},
	"required":             []any{"operation"},
	"additionalProperties": false,
}

// LowerCodexExtensionTools rewrites the extension tools, their replayed calls
// and tool_choice in place, and returns the identities to restore. It builds
// new slices and maps throughout: the caller keeps the original payload for
// stored input items.
func LowerCodexExtensionTools(payload map[string]any) (ToolNamespaces, error) {
	aliases := ToolNamespaces{}
	if rows, ok := payload["tools"].([]any); ok {
		lowered := make([]any, 0, len(rows))
		for i, raw := range rows {
			tool, ok := raw.(map[string]any)
			if !ok {
				lowered = append(lowered, raw)
				continue
			}
			converted, keep, err := lowerCodexTool(tool, i, aliases)
			if err != nil {
				return nil, err
			}
			if keep {
				lowered = append(lowered, converted)
			}
		}
		payload["tools"] = lowered
	}
	var loaded []map[string]any
	if items, ok := payload["input"].([]any); ok {
		lowered := make([]any, 0, len(items))
		for i, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				lowered = append(lowered, raw)
				continue
			}
			converted, keep, err := lowerCodexItem(item, i, aliases)
			if err != nil {
				return nil, err
			}
			if keep {
				lowered = append(lowered, converted)
			}
			if (item["type"] == "tool_search_output" && isClientExecution(item)) || item["type"] == "additional_tools" {
				loaded = append(loaded, InterfaceMaps(item["tools"])...)
			}
		}
		payload["input"] = lowered
	}
	if len(loaded) > 0 {
		if err := declareSearchedTools(payload, loaded, aliases); err != nil {
			return nil, err
		}
	}
	if choice, ok := payload["tool_choice"].(map[string]any); ok {
		switch choice["type"] {
		case toolKindCustom:
			if name, _ := choice["name"].(string); strings.TrimSpace(name) != "" {
				payload["tool_choice"] = map[string]any{"type": "function", "name": name}
			}
		case toolKindApplyPatch:
			payload["tool_choice"] = map[string]any{"type": "function", "name": applyPatchWireName}
		case "tool_search":
			// The search is not a callable tool once deferred functions are
			// loaded eagerly. (Codex always sends tool_choice "auto".)
			payload["tool_choice"] = "auto"
		}
	}
	return aliases, nil
}

func lowerCodexTool(tool map[string]any, i int, aliases ToolNamespaces) (map[string]any, bool, error) {
	switch tool["type"] {
	case toolKindCustom:
		name, _ := tool["name"].(string)
		if strings.TrimSpace(name) == "" {
			return nil, false, fmt.Errorf("tools[%d].name is required", i)
		}
		if err := addCodexAlias(aliases, name, namespaceToolIdentity{Kind: toolKindCustom, Name: name, Function: CloneStringInterfaceMap(tool)}); err != nil {
			return nil, false, fmt.Errorf("tools[%d]: %w", i, err)
		}
		return map[string]any{
			"type": "function", "name": name, "description": customToolDescription(tool),
			"parameters": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"input": map[string]any{"type": "string"}},
				"required":             []any{"input"},
				"additionalProperties": false,
			},
		}, true, nil
	case toolKindApplyPatch:
		if err := addCodexAlias(aliases, applyPatchWireName, namespaceToolIdentity{Kind: toolKindApplyPatch, Name: applyPatchWireName, Function: CloneStringInterfaceMap(tool)}); err != nil {
			return nil, false, fmt.Errorf("tools[%d]: %w", i, err)
		}
		return map[string]any{
			"type": "function", "name": applyPatchWireName,
			"description": "Create, update, or delete one file using a structured V4A patch operation. " +
				"create_file and update_file require path and diff; delete_file requires path.",
			"parameters": applyPatchParameters,
		}, true, nil
	case "tool_search":
		// The upstream has no tool search. Server-side search is answered by
		// loading every deferred function up front. A client-side search is a
		// function the client executes: its tool_search_output comes back on
		// the next call, and the definitions in it are declared then.
		if !isClientExecution(tool) {
			return nil, false, nil
		}
		if err := addCodexAlias(aliases, toolSearchWireName, namespaceToolIdentity{Kind: toolKindToolSearch, Name: toolSearchWireName, Function: CloneStringInterfaceMap(tool)}); err != nil {
			return nil, false, fmt.Errorf("tools[%d]: %w", i, err)
		}
		description, _ := tool["description"].(string)
		parameters, ok := tool["parameters"].(map[string]any)
		if !ok {
			parameters = toolSearchParameters
		}
		return map[string]any{"type": "function", "name": toolSearchWireName, "description": description, "parameters": parameters}, true, nil
	case "function":
		return withoutDeferLoading(tool), true, nil
	case "namespace":
		return lowerCodexNamespaceTool(tool, i, aliases)
	}
	return tool, true, nil
}

func lowerCodexNamespaceTool(tool map[string]any, i int, aliases ToolNamespaces) (map[string]any, bool, error) {
	children, ok := tool["tools"].([]any)
	if !ok {
		return tool, true, nil
	}
	lowered := CloneStringInterfaceMap(tool)
	copied := make([]any, 0, len(children))
	for j, raw := range children {
		child, ok := raw.(map[string]any)
		if !ok {
			copied = append(copied, raw)
			continue
		}
		converted, keep, err := lowerCodexTool(child, j, aliases)
		if err != nil {
			return nil, false, fmt.Errorf("tools[%d].tools[%d]: %w", i, j, err)
		}
		if keep {
			copied = append(copied, converted)
		}
	}
	lowered["tools"] = copied
	return lowered, true, nil
}

func lowerCodexItem(item map[string]any, i int, aliases ToolNamespaces) (map[string]any, bool, error) {
	callID, _ := item["call_id"].(string)
	switch item["type"] {
	case "custom_tool_call":
		name, _ := item["name"].(string)
		input, ok := item["input"].(string)
		if strings.TrimSpace(callID) == "" || strings.TrimSpace(name) == "" || !ok {
			return nil, false, fmt.Errorf("input[%d]: custom_tool_call requires call_id, name and a string input", i)
		}
		if _, exists := aliases[name]; !exists {
			aliases[name] = namespaceToolIdentity{Kind: toolKindCustom, Name: name}
		}
		arguments, _ := json.Marshal(map[string]any{"input": input})
		return map[string]any{"type": "function_call", "call_id": callID, "name": name, "arguments": string(arguments)}, true, nil
	case "apply_patch_call":
		operation, ok := item["operation"].(map[string]any)
		if strings.TrimSpace(callID) == "" || !ok {
			return nil, false, fmt.Errorf("input[%d]: apply_patch_call requires call_id and an operation object", i)
		}
		if _, exists := aliases[applyPatchWireName]; !exists {
			aliases[applyPatchWireName] = namespaceToolIdentity{Kind: toolKindApplyPatch, Name: applyPatchWireName}
		}
		arguments, err := json.Marshal(map[string]any{"operation": operation})
		if err != nil {
			return nil, false, fmt.Errorf("input[%d]: %w", i, err)
		}
		return map[string]any{"type": "function_call", "call_id": callID, "name": applyPatchWireName, "arguments": string(arguments)}, true, nil
	case "custom_tool_call_output", "apply_patch_call_output":
		if strings.TrimSpace(callID) == "" {
			return nil, false, fmt.Errorf("input[%d]: %s requires call_id", i, item["type"])
		}
		output, exists := item["output"]
		if !exists || output == nil {
			// apply_patch_call_output may report only a status.
			status, _ := item["status"].(string)
			output = status
		}
		return map[string]any{"type": "function_call_output", "call_id": callID, "output": output}, true, nil
	case "additional_tools":
		// Codex declares tools part-way through a conversation with this item.
		// The definitions are declared for the turn (LowerCodexExtensionTools);
		// the item itself has no Chat or upstream equivalent.
		return nil, false, nil
	case "function_call_output":
		// Codex also sends tool output nobody called for, such as a plugin's
		// notifications: no call_id, identified by name and namespace. A tool
		// result must answer a call, so it reaches the model as a user message
		// naming its source, rather than as an invented call.
		name := chatwire.ParseLooseStringAny(item["name"])
		if strings.TrimSpace(callID) != "" || strings.TrimSpace(name) == "" {
			return item, true, nil
		}
		return unpairedToolOutputMessage(item, name), true, nil
	case "tool_search_call", "tool_search_output":
		if !isClientExecution(item) {
			// Deferred functions are loaded eagerly, so a recorded server
			// search carries nothing the upstream needs.
			return nil, false, nil
		}
		if strings.TrimSpace(callID) == "" {
			return nil, false, fmt.Errorf("input[%d]: client %s requires call_id", i, item["type"])
		}
		if _, exists := aliases[toolSearchWireName]; !exists {
			aliases[toolSearchWireName] = namespaceToolIdentity{Kind: toolKindToolSearch, Name: toolSearchWireName}
		}
		if item["type"] == "tool_search_call" {
			arguments, err := json.Marshal(item["arguments"])
			if err != nil || string(arguments) == "null" {
				arguments = []byte("{}")
			}
			return map[string]any{"type": "function_call", "call_id": callID, "name": toolSearchWireName, "arguments": string(arguments)}, true, nil
		}
		return map[string]any{"type": "function_call_output", "call_id": callID, "output": searchedToolsSummary(InterfaceMaps(item["tools"]))}, true, nil
	}
	return item, true, nil
}

// unpairedToolOutputMessage renders an uncalled tool output as a user message:
// a label naming the tool, then the output text or its content parts.
func unpairedToolOutputMessage(item map[string]any, name string) map[string]any {
	source := name
	if namespace := strings.TrimSpace(chatwire.ParseLooseStringAny(item["namespace"])); namespace != "" {
		source = namespace + "." + name
	}
	label := "Tool output from " + source + " (sent by the client, not a reply to a tool call):"
	parts := []any{map[string]any{"type": "input_text", "text": label}}
	switch output := item["output"].(type) {
	case string:
		parts[0] = map[string]any{"type": "input_text", "text": label + "\n" + output}
	case []any:
		parts = append(parts, output...)
	default:
		if output != nil {
			encoded, _ := json.Marshal(output)
			parts[0] = map[string]any{"type": "input_text", "text": label + "\n" + string(encoded)}
		}
	}
	return map[string]any{"type": "message", "role": "user", "content": parts}
}

// toolSearchParameters is Codex's own search argument shape, used when a
// declaration carries none.
var toolSearchParameters = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"query": map[string]any{"type": "string", "description": "Search query for deferred tools."},
		"limit": map[string]any{"type": "number", "description": "Maximum number of tools to return."},
	},
	"required":             []any{"query"},
	"additionalProperties": false,
}

func isClientExecution(value map[string]any) bool {
	execution, _ := value["execution"].(string)
	return strings.EqualFold(strings.TrimSpace(execution), "client")
}

// declareSearchedTools adds the definitions a client tool search returned to
// the request's tools, so the upstream can call them. A name already declared
// (by the client, or by an earlier search in the history) is kept once.
func declareSearchedTools(payload map[string]any, loaded []map[string]any, aliases ToolNamespaces) error {
	tools, _ := payload["tools"].([]any)
	declared := map[string]bool{}
	for _, tool := range InterfaceMaps(tools) {
		declared[chatwire.ParseLooseStringAny(tool["type"])+"\x00"+chatwire.ParseLooseStringAny(tool["name"])] = true
	}
	for i, tool := range loaded {
		key := chatwire.ParseLooseStringAny(tool["type"]) + "\x00" + chatwire.ParseLooseStringAny(tool["name"])
		if declared[key] {
			continue
		}
		declared[key] = true
		converted, keep, err := lowerCodexTool(tool, i, aliases)
		if err != nil {
			return fmt.Errorf("tool_search_output: %w", err)
		}
		if keep {
			tools = append(tools, converted)
		}
	}
	payload["tools"] = tools
	return nil
}

// searchedToolsSummary is what the model reads as the search result: the
// tools it can now call. Their definitions travel as declarations.
func searchedToolsSummary(tools []map[string]any) string {
	if len(tools) == 0 {
		return "No matching tools were found."
	}
	var names []string
	for _, tool := range tools {
		name := chatwire.ParseLooseStringAny(tool["name"])
		if tool["type"] == "namespace" {
			var children []string
			for _, child := range InterfaceMaps(tool["tools"]) {
				children = append(children, chatwire.ParseLooseStringAny(child["name"]))
			}
			name += " (" + strings.Join(children, ", ") + ")"
		}
		names = append(names, name)
	}
	return "Loaded tools, now available to call: " + strings.Join(names, "; ")
}

func addCodexAlias(aliases ToolNamespaces, name string, identity namespaceToolIdentity) error {
	if existing, exists := aliases[name]; exists && existing.Function != nil {
		return fmt.Errorf("duplicate tool name %q", name)
	}
	aliases[name] = identity
	return nil
}

func withoutDeferLoading(tool map[string]any) map[string]any {
	if _, exists := tool["defer_loading"]; !exists {
		return tool
	}
	copied := CloneStringInterfaceMap(tool)
	delete(copied, "defer_loading")
	return copied
}

// customToolDescription keeps the client's description and states the input
// contract. A grammar format cannot be enforced on a function argument, so its
// definition is handed to the model as text.
func customToolDescription(tool map[string]any) string {
	description, _ := tool["description"].(string)
	parts := []string{strings.TrimSpace(description)}
	if format, ok := tool["format"].(map[string]any); ok && format["type"] == "grammar" {
		if definition, _ := format["definition"].(string); strings.TrimSpace(definition) != "" {
			if len(definition) > maxGrammarDescription {
				definition = definition[:maxGrammarDescription]
			}
			syntax, _ := format["syntax"].(string)
			parts = append(parts, fmt.Sprintf("The input must follow this %s grammar:\n%s", strings.TrimSpace(syntax), definition))
		}
	}
	parts = append(parts, "Provide the tool input as the input string field.")
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// raiseCodexItem turns a lowered function_call back into the client's item.
// The arguments may be empty or partial on an in-progress item; the fields are
// then left out rather than guessed.
func raiseCodexItem(item map[string]any, identity namespaceToolIdentity) {
	arguments, _ := item["arguments"].(string)
	delete(item, "arguments")
	delete(item, "namespace")
	switch identity.Kind {
	case toolKindCustom:
		item["type"] = "custom_tool_call"
		item["name"] = identity.Name
		var decoded struct {
			Input *string `json:"input"`
		}
		item["input"] = ""
		if json.Unmarshal([]byte(arguments), &decoded) == nil && decoded.Input != nil {
			item["input"] = *decoded.Input
		}
	case toolKindToolSearch:
		item["type"] = "tool_search_call"
		item["execution"] = "client"
		delete(item, "name")
		var decoded any
		if json.Unmarshal([]byte(arguments), &decoded) == nil && decoded != nil {
			item["arguments"] = decoded
		} else {
			item["arguments"] = map[string]any{}
		}
	case toolKindApplyPatch:
		item["type"] = "apply_patch_call"
		delete(item, "name")
		var decoded struct {
			Operation map[string]any `json:"operation"`
		}
		if json.Unmarshal([]byte(arguments), &decoded) == nil && decoded.Operation != nil {
			item["operation"] = decoded.Operation
		}
	}
}

// codexWireName is the function name a lowered identity is sent under.
func codexWireName(identity namespaceToolIdentity) string {
	switch identity.Kind {
	case toolKindApplyPatch:
		return applyPatchWireName
	case toolKindToolSearch:
		return toolSearchWireName
	}
	return identity.Name
}

// LoweredCallFilter drops the argument stream of lowered calls. Their
// arguments are the function envelope, not the client's input or patch, so the
// client receives the call whole on response.output_item.done instead.
type LoweredCallFilter struct {
	aliases ToolNamespaces
	items   map[string]bool
}

func NewLoweredCallFilter(aliases ToolNamespaces) *LoweredCallFilter {
	return &LoweredCallFilter{aliases: aliases, items: map[string]bool{}}
}

// Drop reports whether an upstream event must not reach the client. It must
// see each event before RestoreEnvelope rewrites the item type.
func (f *LoweredCallFilter) Drop(event map[string]any) bool {
	if f == nil || len(f.aliases) == 0 {
		return false
	}
	switch event["type"] {
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		if item["type"] != "function_call" {
			return false
		}
		name, _ := item["name"].(string)
		if identity, ok := f.aliases[name]; ok && identity.Kind != "" {
			if id, _ := item["id"].(string); id != "" {
				f.items[id] = true
			}
		}
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		id, _ := event["item_id"].(string)
		return f.items[id]
	}
	return false
}
