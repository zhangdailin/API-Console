package responses

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

type namespaceToolIdentity struct {
	Namespace   string                 `json:"namespace"`
	Name        string                 `json:"name"`
	Function    map[string]interface{} `json:"function,omitempty"`
	Description string                 `json:"description,omitempty"`
}

// ToolNamespaces keeps the client identity of each flattened function for one
// request and its continuation. Schemas and arguments stay intact.
type ToolNamespaces map[string]namespaceToolIdentity

// A stable suffix prevents collisions between groups, sanitized names and plain
// functions, even when the tool set changes on a continuation. Fit Chat's 64-byte
// function-name limit without relying on declaration order.
func namespaceToolName(namespace, name string) string {
	digest := sha256.Sum256([]byte(namespace + "\x00" + name))
	var prefix strings.Builder
	for _, c := range namespace + "__" + name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			prefix.WriteRune(c)
		} else {
			prefix.WriteByte('_')
		}
		if prefix.Len() == 47 {
			break
		}
	}
	return fmt.Sprintf("%s_%x", prefix.String(), digest[:8])
}

// NormalizeBridgedNamespaces adapts Codex grouping to flat function declarations
// for Chat and native upstreams. Unsupported children still fail; this does not emulate
// custom tools, execute tools, or change their parameter JSON.
func NormalizeBridgedNamespaces(req *CreateRequest) (ToolNamespaces, error) {
	aliases := ToolNamespaces{}
	normalized := make([]map[string]interface{}, 0, len(req.Tools))
	seenNamespaces, seenNames := map[string]bool{}, map[string]bool{}
	for i, tool := range req.Tools {
		if tool["type"] != "namespace" {
			normalized = append(normalized, tool)
			continue
		}
		namespace, ok := tool["name"].(string)
		if !ok || strings.TrimSpace(namespace) == "" {
			return nil, fmt.Errorf("tools[%d].name is required", i)
		}
		if seenNamespaces[namespace] {
			return nil, fmt.Errorf("tools[%d]: duplicate namespace %q", i, namespace)
		}
		seenNamespaces[namespace] = true
		children := InterfaceMaps(tool["tools"])
		valid := false
		switch rows := tool["tools"].(type) {
		case []map[string]interface{}:
			valid = len(rows) > 0
		case []interface{}:
			valid = len(rows) > 0 && len(rows) == len(children)
		}
		if !valid {
			return nil, fmt.Errorf("tools[%d].tools must be a nonempty array of function objects", i)
		}
		for j, child := range children {
			if child["type"] != "function" {
				return nil, fmt.Errorf("tools[%d].tools[%d]: only function tools can be bridged", i, j)
			}
			// Apply the same standard-function checks before removing the grouping.
			if err := ValidateToolsAndHistory(map[string]interface{}{"tools": []map[string]interface{}{child}}); err != nil {
				return nil, fmt.Errorf("tools[%d].tools[%d]: %w", i, j, err)
			}
			name := child["name"].(string)
			alias := namespaceToolName(namespace, name)
			if seenNames[alias] {
				return nil, fmt.Errorf("tools[%d].tools[%d]: duplicate function identity", i, j)
			}
			seenNames[alias] = true
			description, _ := tool["description"].(string)
			aliases[alias] = namespaceToolIdentity{Namespace: namespace, Name: name, Function: CloneStringInterfaceMap(child), Description: description}
			flat := CloneStringInterfaceMap(child)
			flat["name"] = alias
			if description, _ := tool["description"].(string); description != "" {
				childDescription, _ := child["description"].(string)
				flat["description"] = strings.TrimSpace(description + "\n" + childDescription)
			}
			normalized = append(normalized, flat)
		}
	}
	// All name collisions, including a plain function named like an alias, are
	// rejected by the normal validator after flattening.
	input := req.Input
	if items, ok := input.([]interface{}); ok {
		copyItems := append([]interface{}(nil), items...)
		for i, raw := range items {
			item, ok := raw.(map[string]interface{})
			if !ok || item["type"] != "function_call" {
				continue
			}
			namespace, exists := item["namespace"]
			if !exists {
				continue
			}
			ns, nsOK := namespace.(string)
			name, nameOK := item["name"].(string)
			if !nsOK || strings.TrimSpace(ns) == "" || !nameOK || strings.TrimSpace(name) == "" {
				return nil, fmt.Errorf("input[%d]: namespace and name must be nonempty strings", i)
			}
			lowered := CloneStringInterfaceMap(item)
			alias := namespaceToolName(ns, name)
			lowered["name"] = alias
			if _, exists := aliases[alias]; !exists {
				aliases[alias] = namespaceToolIdentity{Namespace: ns, Name: name}
			}
			delete(lowered, "namespace")
			copyItems[i] = lowered
		}
		input = copyItems
	}
	choice := req.ToolChoice
	if original, ok := choice.(map[string]interface{}); ok {
		if namespace, exists := original["namespace"]; exists {
			ns, nsOK := namespace.(string)
			name, nameOK := original["name"].(string)
			if original["type"] != "function" || !nsOK || !nameOK || strings.TrimSpace(ns) == "" || strings.TrimSpace(name) == "" {
				return nil, fmt.Errorf("tool_choice namespace requires a function name")
			}
			copyChoice := CloneStringInterfaceMap(original)
			copyChoice["name"] = namespaceToolName(ns, name)
			delete(copyChoice, "namespace")
			choice = copyChoice
		}
	}
	if err := ValidateToolsAndHistory(map[string]interface{}{"tools": normalized, "input": input, "tool_choice": choice}); err != nil {
		return nil, err
	}
	if err := aliases.ValidatePlainTools(req.Tools); err != nil {
		return nil, err
	}
	req.Tools, req.Input, req.ToolChoice = normalized, input, choice
	return aliases, nil
}

func (aliases ToolNamespaces) RestoreItem(item map[string]interface{}) bool {
	if item["type"] != "function_call" {
		return false
	}
	name, _ := item["name"].(string)
	if identity, ok := aliases[name]; ok {
		item["name"], item["namespace"] = identity.Name, identity.Namespace
		return true
	}
	return false
}

func (aliases ToolNamespaces) RestoreResponse(response map[string]interface{}) bool {
	changed := false
	for _, item := range InterfaceMaps(response["output"]) {
		changed = aliases.RestoreItem(item) || changed
	}
	return changed
}
