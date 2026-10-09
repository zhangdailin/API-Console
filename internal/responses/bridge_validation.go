package responses

import (
	"fmt"
	"strings"
)

// ValidateToolsAndHistory accepts native declarations without rewriting their
// names, parameter schemas, argument JSON, or replay items.
func ValidateToolsAndHistory(payload map[string]interface{}) error {
	tools := InterfaceMaps(payload["tools"])
	if raw := payload["tools"]; raw != nil {
		valid := false
		switch rows := raw.(type) {
		case []map[string]interface{}:
			valid = true
		case []interface{}:
			valid = len(rows) == len(tools)
		}
		if !valid {
			return fmt.Errorf("tools must be an array of objects")
		}
	}
	seen := map[string]bool{}
	for i, tool := range tools {
		kind, _ := tool["type"].(string)
		switch kind {
		case "function":
			name, _ := tool["name"].(string)
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("tools[%d].name is required", i)
			}
			if seen[name] {
				return fmt.Errorf("tools[%d]: duplicate function name %q", i, name)
			}
			seen[name] = true
			for _, field := range []string{"namespace", "function", "defer_loading", "aliases"} {
				if _, exists := tool[field]; exists {
					return fmt.Errorf("tools[%d].%s is not supported", i, field)
				}
			}
		case "web_search", "x_search", "mcp", "shell", "image_generation", "collections_search", "file_search", "code_execution", "code_interpreter":
		default:
			return fmt.Errorf("tools[%d]: tool type %q is not supported", i, kind)
		}
	}
	if choice, ok := payload["tool_choice"].(map[string]interface{}); ok {
		if _, exists := choice["namespace"]; exists {
			return fmt.Errorf("tool_choice.namespace is not supported")
		}
		if _, exists := choice["function"]; exists {
			return fmt.Errorf("tool_choice must use the Responses function format")
		}
		kind, _ := choice["type"].(string)
		switch kind {
		case "function":
			name, _ := choice["name"].(string)
			if !seen[name] {
				return fmt.Errorf("tool_choice function %q is not declared", name)
			}
		case "web_search", "x_search", "mcp", "shell", "image_generation", "collections_search", "file_search", "code_execution", "code_interpreter":
		default:
			return fmt.Errorf("tool_choice type %q is not supported", kind)
		}
	}
	for i, item := range InterfaceMaps(payload["input"]) {
		kind, _ := item["type"].(string)
		switch kind {
		case "agent_message", "local_shell_call", "local_shell_call_output", "mcp_tool_call_output", "custom_tool_call", "custom_tool_call_output", "apply_patch_call", "apply_patch_call_output", "tool_search_call", "tool_search_output":
			return fmt.Errorf("input[%d]: item type %q is not supported", i, kind)
		case "function_call":
			if _, exists := item["namespace"]; exists {
				return fmt.Errorf("input[%d].namespace is not supported", i)
			}
		}
	}
	return nil
}

// ValidateBridgedTools checks the declaration and replay contract before the
// existing standard Responses-to-Chat conversion.
func ValidateBridgedTools(req *CreateRequest) error {
	if err := ValidateToolsAndHistory(map[string]interface{}{"tools": req.Tools, "tool_choice": req.ToolChoice, "input": req.Input}); err != nil {
		return err
	}
	for i, tool := range req.Tools {
		kind, _ := tool["type"].(string)
		if kind != "function" {
			return fmt.Errorf("tools[%d]: tool type %q requires a native Responses provider", i, kind)
		}
	}
	return nil
}
