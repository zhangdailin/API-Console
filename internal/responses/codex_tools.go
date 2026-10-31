package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Codex declares tools a function-only upstream cannot accept: freeform
// `custom` tools, the structured `apply_patch` tool, and `tool_search` with
// deferred (`defer_loading`) functions. The client executes all of them, so
// each is lowered to an ordinary function with a fixed argument shape and the
// call is raised back to the client's item type on the way out. The identity
// rides in ToolNamespaces, so it is persisted and restored by the same
// plumbing as namespace aliases.
const (
	toolKindCustom     = "custom"
	toolKindApplyPatch = "apply_patch"
	applyPatchWireName = "apply_patch"
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
		}
		payload["input"] = lowered
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
			// loaded eagerly.
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
		// loading every deferred function up front; a client-side search needs
		// the client to supply definitions mid-turn, which cannot be emulated.
		if execution, _ := tool["execution"].(string); strings.EqualFold(strings.TrimSpace(execution), "client") {
			return nil, false, fmt.Errorf("tools[%d]: client-executed tool_search is not supported by this upstream", i)
		}
		return nil, false, nil
	case "function":
		return withoutDeferLoading(tool), true, nil
	case "namespace":
		children, ok := tool["tools"].([]any)
		if !ok {
			return tool, true, nil
		}
		lowered := CloneStringInterfaceMap(tool)
		copied := make([]any, len(children))
		for j, raw := range children {
			if child, ok := raw.(map[string]any); ok {
				copied[j] = withoutDeferLoading(child)
			} else {
				copied[j] = raw
			}
		}
		lowered["tools"] = copied
		return lowered, true, nil
	}
	return tool, true, nil
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
	case "tool_search_call", "tool_search_output":
		// Deferred functions are loaded eagerly, so a recorded search carries
		// nothing the upstream needs.
		return nil, false, nil
	}
	return item, true, nil
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
	if identity.Kind == toolKindApplyPatch {
		return applyPatchWireName
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
