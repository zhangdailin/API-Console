package responses

import (
	"encoding/json"
	"fmt"
)

// NormalizeNamespacePayload preserves native fields outside the tool contract.
// Both native Responses and Chat transports use the same stable function names.
func NormalizeNamespacePayload(payload map[string]any) (ToolNamespaces, error) {
	tools := InterfaceMaps(payload["tools"])
	if raw := payload["tools"]; raw != nil {
		switch rows := raw.(type) {
		case []any:
			if len(rows) != len(tools) {
				return nil, fmt.Errorf("tools must contain only objects")
			}
		case []map[string]any:
		default:
			return nil, fmt.Errorf("tools must be an array of objects")
		}
	}
	// Codex extension tools are lowered first: the namespace adaptation below
	// validates the result and would refuse them.
	lowered, err := LowerCodexExtensionTools(payload)
	if err != nil {
		return nil, err
	}
	tools = InterfaceMaps(payload["tools"])
	req := CreateRequest{Tools: tools, Input: payload["input"], ToolChoice: payload["tool_choice"]}
	aliases, err := NormalizeBridgedNamespaces(&req)
	if err != nil {
		return nil, err
	}
	for name, identity := range lowered {
		aliases[name] = identity
	}
	for key, value := range map[string]any{"tools": req.Tools, "input": req.Input, "tool_choice": req.ToolChoice} {
		if _, exists := payload[key]; exists {
			payload[key] = value
		}
	}
	return aliases, nil
}

// A plain function cannot reuse a wire name from an earlier namespace: the
// upstream's call would otherwise have two possible public identities.
func (aliases ToolNamespaces) ValidatePlainTools(tools any) error {
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
		switch identity.Kind {
		case "":
			if identity.Namespace == "" || identity.Name == "" || name != namespaceToolName(identity.Namespace, identity.Name) {
				return fmt.Errorf("invalid stored namespace identity")
			}
		case toolKindCustom, toolKindApplyPatch:
			if identity.Namespace != "" || identity.Name == "" || name != codexWireName(identity) {
				return fmt.Errorf("invalid stored tool identity")
			}
		default:
			return fmt.Errorf("invalid stored tool identity")
		}
		if current, exists := aliases[name]; exists {
			if current.Kind != identity.Kind || current.Namespace != identity.Namespace || current.Name != identity.Name {
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
func (aliases ToolNamespaces) RestoreEnvelope(envelope map[string]any) bool {
	changed := aliases.RestoreResponse(envelope)
	if item, ok := envelope["item"].(map[string]any); ok {
		changed = aliases.RestoreItem(item) || changed
	}
	if response, ok := envelope["response"].(map[string]any); ok {
		changed = aliases.RestoreEnvelope(response) || changed
	}
	if choice, ok := envelope["tool_choice"].(map[string]any); ok && choice["type"] == "function" {
		if identity, found := aliases[ParseLooseStringAny(choice["name"])]; found {
			switch identity.Kind {
			case toolKindCustom:
				envelope["tool_choice"] = map[string]any{"type": toolKindCustom, "name": identity.Name}
			case toolKindApplyPatch:
				envelope["tool_choice"] = map[string]any{"type": toolKindApplyPatch}
			default:
				choice["name"], choice["namespace"] = identity.Name, identity.Namespace
			}
			changed = true
		}
	}
	tools := InterfaceMaps(envelope["tools"])
	groups := map[string]map[string]any{}
	var restored []map[string]any
	toolsChanged := false
	for _, tool := range tools {
		identity, found := aliases[ParseLooseStringAny(tool["name"])]
		if tool["type"] != "function" || !found {
			restored = append(restored, tool)
			continue
		}
		toolsChanged = true
		if identity.Kind != "" {
			// A lowered tool is echoed as the client declared it.
			original := CloneStringInterfaceMap(identity.Function)
			if original == nil {
				original = map[string]any{"type": identity.Kind}
				if identity.Kind == toolKindCustom {
					original["name"] = identity.Name
				}
			}
			restored = append(restored, original)
			continue
		}
		group := groups[identity.Namespace]
		if group == nil {
			group = map[string]any{"type": "namespace", "name": identity.Namespace, "tools": []map[string]any{}}
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
		group["tools"] = append(group["tools"].([]map[string]any), function)
	}
	if toolsChanged {
		envelope["tools"] = restored
		changed = true
	}
	return changed
}
