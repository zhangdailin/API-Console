package chatwire

import (
	"fmt"
	"strings"
)

func (r *Request) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("model is required")
	}
	if len(r.Messages) == 0 {
		return fmt.Errorf("messages is required")
	}
	if err := ValidateMessages(r.Messages); err != nil {
		return err
	}
	if err := ValidateToolDefinitions(r.Tools); err != nil {
		return err
	}
	if err := ValidateToolChoice(r.ToolChoice, r.toolChoiceNameSet); err != nil {
		return err
	}
	return nil
}

// ValidateToolChoice checks a Chat Completions tool_choice against the tools a
// request declares; names is only called when the choice needs it.
func ValidateToolChoice(choice any, names func() map[string]struct{}) error {
	if choice != nil {
		switch v := choice.(type) {
		case string:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "auto", "none":
			case "required":
				if len(names()) == 0 {
					return fmt.Errorf("tool_choice required needs at least one defined tool")
				}
			default:
				return fmt.Errorf("tool_choice must be auto, required, none, or a specific function object")
			}
		case map[string]any:
			fn, _ := v["function"].(map[string]any)
			name, _ := fn["name"].(string)
			name = strings.TrimSpace(name)
			if strings.TrimSpace(fmt.Sprint(v["type"])) != "function" || name == "" {
				return fmt.Errorf("tool_choice object must have type=function and function.name")
			}
			// Hosted tools (web_search / x_search) live in ResponsesTools, not
			// in the function list, so both sets decide whether the name exists.
			if _, found := names()[name]; !found {
				return fmt.Errorf("tool_choice.function.name must reference a defined tool")
			}
		default:
			return fmt.Errorf("tool_choice must be auto, required, none, or a specific function object")
		}
	}
	return nil
}

func (r *Request) toolChoiceNameSet() map[string]struct{} {
	if r == nil {
		return nil
	}
	return ToolChoiceNames(r.Tools, r.ResponsesTools)
}

// ToolChoiceNames collects every tool name a tool_choice may refer to: function
// names, hosted tool types, and the names and types of Responses-shaped tools.
func ToolChoiceNames(tools []ToolDef, responsesTools []map[string]any) map[string]struct{} {
	names := make(map[string]struct{}, len(tools)+len(responsesTools))
	for _, tool := range tools {
		if tool.Function == nil {
			if normalized, native := NativeToolTypes[strings.ToLower(strings.TrimSpace(tool.Type))]; native {
				names[normalized] = struct{}{}
			}
			continue
		}
		if name := strings.TrimSpace(fmt.Sprint(tool.Function["name"])); name != "" && name != "<nil>" {
			names[name] = struct{}{}
		}
	}
	for _, tool := range responsesTools {
		for _, key := range []string{"name", "type"} {
			if name := strings.TrimSpace(ParseLooseStringAny(tool[key])); name != "" {
				names[name] = struct{}{}
			}
		}
	}
	return names
}
