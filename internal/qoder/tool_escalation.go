package qoder

import (
	"encoding/json"
	"strings"
)

// normalizeCommandEscalation drops an orphan approval explanation from known
// command tools. It never changes the command or invents an escalation request.
func normalizeCommandEscalation(name, arguments string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndex(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	switch name {
	case "bash", "exec_command", "run_command", "execute_command", "run_terminal_command", "run_shell_command":
	default:
		return arguments
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &fields) != nil {
		return arguments
	}
	if _, exists := fields["justification"]; !exists {
		return arguments
	}
	var command string
	if json.Unmarshal(fields["cmd"], &command) != nil || strings.TrimSpace(command) == "" {
		if json.Unmarshal(fields["command"], &command) != nil || strings.TrimSpace(command) == "" {
			return arguments
		}
	}
	if permission, exists := fields["sandbox_permissions"]; exists {
		var value string
		if string(permission) != "null" && (json.Unmarshal(permission, &value) != nil || strings.TrimSpace(value) != "") {
			return arguments
		}
	}
	delete(fields, "justification")
	encoded, err := json.Marshal(fields)
	if err != nil {
		return arguments
	}
	return string(encoded)
}
