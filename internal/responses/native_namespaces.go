package responses

import (
	"encoding/json"
	"fmt"
)

// NormalizeNamespacePayload preserves native fields outside the tool contract.
// Both native Responses and Chat transports use the same stable function names.
func NormalizeNamespacePayload(payload map[string]interface{}) (ToolNamespaces, error) {
	tools := InterfaceMaps(payload["tools"])
	if raw := payload["tools"]; raw != nil {
		switch rows := raw.(type) {
		case []interface{}:
			if len(rows) != len(tools) {
				return nil, fmt.Errorf("tools must contain only objects")
			}
		case []map[string]interface{}:
		default:
			return nil, fmt.Errorf("tools must be an array of objects")
		}
	}
	req := CreateRequest{Tools: tools, Input: payload["input"], ToolChoice: payload["tool_choice"]}
	aliases, err := NormalizeBridgedNamespaces(&req)
	if err != nil {
		return nil, err
	}
	for key, value := range map[string]interface{}{"tools": req.Tools, "input": req.Input, "tool_choice": req.ToolChoice} {
		if _, exists := payload[key]; exists {
			payload[key] = value
		}
	}
	return aliases, nil
}

// A plain function cannot reuse a wire name from an earlier namespace: the
// upstream's call would otherwise have two possible public identities.
func (aliases ToolNamespaces) ValidatePlainTools(tools interface{}) error {
	for _, tool := range InterfaceMaps(tools) {
		if tool["type"] == "function" {
			if _, exists := aliases[ParseLooseStringAny(tool["name"])]; exists {
				return fmt.Errorf("function name conflicts with namespace history")
			}
		}
	}
	return nil
}

// MergeStored validates persisted identities before using them to restore a
// continuation or resource. Records without mappings remain compatible.
func (aliases ToolNamespaces) MergeStored(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var previous ToolNamespaces
	if err := json.Unmarshal(raw, &previous); err != nil {
		return err
	}
	for name, identity := range previous {
		if identity.Namespace == "" || identity.Name == "" || name != namespaceToolName(identity.Namespace, identity.Name) {
			return fmt.Errorf("invalid stored namespace identity")
		}
		if current, exists := aliases[name]; exists {
			if current.Namespace != identity.Namespace || current.Name != identity.Name {
				return fmt.Errorf("conflicting namespace identity")
			}
			if current.Function != nil {
				continue
			}
		}
		aliases[name] = identity
	}
	return nil
}

// RestoreEnvelope changes only protocol identity fields, never argument strings,
// result bodies, text deltas or opaque reasoning content.
func (aliases ToolNamespaces) RestoreEnvelope(envelope map[string]interface{}) bool {
	changed := aliases.RestoreResponse(envelope)
	if item, ok := envelope["item"].(map[string]interface{}); ok {
		changed = aliases.RestoreItem(item) || changed
	}
	if response, ok := envelope["response"].(map[string]interface{}); ok {
		changed = aliases.RestoreEnvelope(response) || changed
	}
	if choice, ok := envelope["tool_choice"].(map[string]interface{}); ok && choice["type"] == "function" {
		if identity, found := aliases[ParseLooseStringAny(choice["name"])]; found {
			choice["name"], choice["namespace"] = identity.Name, identity.Namespace
			changed = true
		}
	}
	tools := InterfaceMaps(envelope["tools"])
	groups := map[string]map[string]interface{}{}
	var restored []map[string]interface{}
	toolsChanged := false
	for _, tool := range tools {
		identity, found := aliases[ParseLooseStringAny(tool["name"])]
		if tool["type"] != "function" || !found {
			restored = append(restored, tool)
			continue
		}
		toolsChanged = true
		group := groups[identity.Namespace]
		if group == nil {
			group = map[string]interface{}{"type": "namespace", "name": identity.Namespace, "tools": []map[string]interface{}{}}
			if identity.Description != "" {
				group["description"] = identity.Description
			}
			groups[identity.Namespace] = group
			restored = append(restored, group)
		}
		function := CloneStringInterfaceMap(identity.Function)
		if function == nil {
			function = CloneStringInterfaceMap(tool)
			function["name"] = identity.Name
		}
		group["tools"] = append(group["tools"].([]map[string]interface{}), function)
	}
	if toolsChanged {
		envelope["tools"] = restored
		changed = true
	}
	return changed
}
