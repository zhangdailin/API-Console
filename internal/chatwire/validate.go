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
	DropBlankUserTextParts(r.Messages)
	if err := ValidateMessages(r.Messages); err != nil {
		return err
	}
	if err := ValidateToolDefinitions(r.Tools); err != nil {
		return err
	}
	return r.ValidateToolChoice()
}

// ValidateToolChoice checks a Chat Completions tool_choice against the tools a
// request declares. Only required and named choices need the list of names.
func (r *Request) ValidateToolChoice() error {
	switch v := r.ToolChoice.(type) {
	case nil:
		return nil
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "auto", "none":
			return nil
		case "required":
			if len(r.toolChoiceNameSet()) == 0 {
				return fmt.Errorf("tool_choice required needs at least one defined tool")
			}
			return nil
		}
	case map[string]any:
		fn, _ := v["function"].(map[string]any)
		name, _ := fn["name"].(string)
		name = strings.TrimSpace(name)
		if strings.TrimSpace(fmt.Sprint(v["type"])) != "function" || name == "" {
			return fmt.Errorf("tool_choice object must have type=function and function.name")
		}
		if _, found := r.toolChoiceNameSet()[name]; !found {
			return fmt.Errorf("tool_choice.function.name must reference a defined tool")
		}
		return nil
	}
	return fmt.Errorf("tool_choice must be auto, required, none, or a specific function object")
}

func (r *Request) toolChoiceNameSet() map[string]struct{} {
	names := make(map[string]struct{}, len(r.Tools)+len(r.ResponsesTools))
	for _, tool := range r.Tools {
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
	for _, tool := range r.ResponsesTools {
		for _, key := range []string{"name", "type"} {
			if name := strings.TrimSpace(ParseLooseStringAny(tool[key])); name != "" {
				names[name] = struct{}{}
			}
		}
	}
	return names
}
