package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

func codexPayload() map[string]any {
	return map[string]any{
		"tools": []any{
			map[string]any{"type": "custom", "name": "exec", "description": "Run a command.",
				"format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: /.+/"}},
			map[string]any{"type": "apply_patch"},
			map[string]any{"type": "tool_search"},
			map[string]any{"type": "function", "name": "lookup", "defer_loading": true, "parameters": map[string]any{"type": "object"}},
			map[string]any{"type": "namespace", "name": "mcp", "tools": []any{
				map[string]any{"type": "function", "name": "read", "defer_loading": true, "parameters": map[string]any{"type": "object"}},
			}},
		},
		"input": []any{
			map[string]any{"type": "custom_tool_call", "id": "ctc_1", "call_id": "c1", "name": "exec", "input": "ls -la"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "c1", "output": "total 0"},
			map[string]any{"type": "apply_patch_call", "call_id": "c2", "status": "completed",
				"operation": map[string]any{"type": "create_file", "path": "a.txt", "diff": "+hi"}},
			map[string]any{"type": "apply_patch_call_output", "call_id": "c2", "status": "completed"},
			map[string]any{"type": "tool_search_call", "call_id": "c3"},
			map[string]any{"type": "tool_search_output", "call_id": "c3"},
		},
		"tool_choice": map[string]any{"type": "custom", "name": "exec"},
	}
}

// TestLowerCodexExtensionToolsProducesFunctionsOnly covers every lowered
// declaration, replayed item and tool_choice, and proves the caller's original
// slices are not modified.
func TestLowerCodexExtensionToolsProducesFunctionsOnly(t *testing.T) {
	payload := codexPayload()
	originalTools := payload["tools"].([]any)
	aliases, err := LowerCodexExtensionTools(payload)
	testutil.NoError(t, err)

	tools := InterfaceMaps(payload["tools"])
	testutil.Equal(t, len(tools), 4)
	custom, patch, lookup, namespace := tools[0], tools[1], tools[2], tools[3]
	testutil.Equal[any](t, custom["type"], "function")
	testutil.Equal[any](t, custom["name"], "exec")
	testutil.MustContain(t, custom["description"].(string), "start: /.+/")
	testutil.Equal[any](t, patch["name"], "apply_patch")
	_, deferred := lookup["defer_loading"]
	testutil.False(t, deferred, "defer_loading must be stripped")
	child := namespace["tools"].([]any)[0].(map[string]any)
	_, deferred = child["defer_loading"]
	testutil.False(t, deferred, "defer_loading must be stripped from namespace children")
	_, deferred = originalTools[3].(map[string]any)["defer_loading"]
	testutil.True(t, deferred, "the caller's declaration must not be modified")

	items := InterfaceMaps(payload["input"])
	testutil.Equal(t, len(items), 4)
	testutil.Equal[any](t, items[0]["type"], "function_call")
	testutil.Equal[any](t, items[0]["arguments"], `{"input":"ls -la"}`)
	_, hasID := items[0]["id"]
	testutil.False(t, hasID, "a client item id must not reach the upstream")
	testutil.Equal[any](t, items[1]["type"], "function_call_output")
	testutil.Equal[any](t, items[2]["name"], "apply_patch")
	testutil.MustContain(t, items[2]["arguments"].(string), `"operation":{"diff":"+hi","path":"a.txt","type":"create_file"}`)
	testutil.Equal[any](t, items[3]["output"], "completed")

	choice := payload["tool_choice"].(map[string]any)
	testutil.Equal[any](t, choice["type"], "function")
	testutil.Equal[any](t, choice["name"], "exec")
	testutil.Equal(t, aliases["exec"].Kind, toolKindCustom)
	testutil.Equal(t, aliases["apply_patch"].Kind, toolKindApplyPatch)
}

func TestLowerCodexExtensionToolsRejectsWhatCannotBeEmulated(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"client tool_search":  {"tools": []any{map[string]any{"type": "tool_search", "execution": "client"}}},
		"duplicate custom":    {"tools": []any{map[string]any{"type": "custom", "name": "x"}, map[string]any{"type": "custom", "name": "x"}}},
		"custom without name": {"tools": []any{map[string]any{"type": "custom"}}},
		"call without input":  {"input": []any{map[string]any{"type": "custom_tool_call", "call_id": "c", "name": "x"}}},
		"patch without op":    {"input": []any{map[string]any{"type": "apply_patch_call", "call_id": "c"}}},
	} {
		_, err := LowerCodexExtensionTools(payload)
		testutil.True(t, err != nil, name+" must be rejected")
	}
}

// TestRaiseCodexItemsRestoresClientShapes covers completed and in-progress
// calls: a partial argument string leaves the payload field out.
func TestRaiseCodexItemsRestoresClientShapes(t *testing.T) {
	payload := codexPayload()
	aliases, err := LowerCodexExtensionTools(payload)
	testutil.NoError(t, err)

	custom := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "c9", "name": "exec", "arguments": `{"input":"pwd"}`, "status": "completed"}
	testutil.True(t, aliases.RestoreItem(custom), "a lowered call must be raised")
	testutil.Equal[any](t, custom["type"], "custom_tool_call")
	testutil.Equal[any](t, custom["input"], "pwd")
	testutil.Equal[any](t, custom["id"], "fc_1")
	_, hasArguments := custom["arguments"]
	testutil.False(t, hasArguments, "arguments must not leak into a custom_tool_call")

	patch := map[string]any{"type": "function_call", "call_id": "c8", "name": "apply_patch",
		"arguments": `{"operation":{"type":"update_file","path":"a.txt","diff":"-a\n+b"}}`, "status": "completed"}
	aliases.RestoreItem(patch)
	testutil.Equal[any](t, patch["type"], "apply_patch_call")
	testutil.Equal[any](t, patch["operation"].(map[string]any)["path"], "a.txt")
	_, hasName := patch["name"]
	testutil.False(t, hasName, "apply_patch_call carries no name")

	partial := map[string]any{"type": "function_call", "call_id": "c7", "name": "apply_patch", "arguments": `{"operation":{"ty`}
	aliases.RestoreItem(partial)
	_, hasOperation := partial["operation"]
	testutil.False(t, hasOperation, "a partial operation must be left out")

	plain := map[string]any{"type": "function_call", "name": "lookup", "arguments": "{}"}
	testutil.False(t, aliases.RestoreItem(plain), "a plain function must be left alone")
}

// TestCodexAliasesRoundTripThroughStorage proves the persisted mapping restores
// a stored resource, echoes the client's declarations and rejects tampering.
func TestCodexAliasesRoundTripThroughStorage(t *testing.T) {
	payload := codexPayload()
	aliases, err := LowerCodexExtensionTools(payload)
	testutil.NoError(t, err)
	raw, err := json.Marshal(aliases)
	testutil.NoError(t, err)

	restored := ToolNamespaces{}
	testutil.NoError(t, restored.MergeStored(raw))
	envelope := map[string]any{
		"tools":       payload["tools"],
		"tool_choice": map[string]any{"type": "function", "name": "apply_patch"},
		"output":      []any{map[string]any{"type": "function_call", "call_id": "c", "name": "exec", "arguments": `{"input":"x"}`}},
	}
	testutil.True(t, restored.RestoreEnvelope(envelope), "the envelope must change")
	tools := InterfaceMaps(envelope["tools"])
	testutil.Equal[any](t, tools[0]["type"], "custom")
	testutil.Equal[any](t, tools[1]["type"], "apply_patch")
	testutil.Equal[any](t, envelope["tool_choice"].(map[string]any)["type"], "apply_patch")
	testutil.Equal[any](t, InterfaceMaps(envelope["output"])[0]["type"], "custom_tool_call")

	for _, tampered := range []string{
		`{"other":{"kind":"custom","namespace":"","name":"exec"}}`,
		`{"exec":{"kind":"custom","namespace":"ns","name":"exec"}}`,
		`{"exec":{"kind":"bogus","namespace":"","name":"exec"}}`,
	} {
		testutil.True(t, (ToolNamespaces{}).MergeStored(json.RawMessage(tampered)) != nil, "tampered mapping must be rejected: "+tampered)
	}
	conflict := ToolNamespaces{"exec": {Kind: toolKindApplyPatch, Name: "apply_patch"}}
	testutil.True(t, conflict.MergeStored(raw) != nil, "a conflicting kind must be rejected")
}

// TestLoweredCallFilterDropsOnlyLoweredArgumentStreams proves the envelope
// argument deltas of a lowered call are withheld and a plain call's are kept.
func TestLoweredCallFilterDropsOnlyLoweredArgumentStreams(t *testing.T) {
	payload := codexPayload()
	aliases, err := LowerCodexExtensionTools(payload)
	testutil.NoError(t, err)
	filter := NewLoweredCallFilter(aliases)
	added := func(id, name string) map[string]any {
		return map[string]any{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": id, "name": name}}
	}
	delta := func(id string) map[string]any {
		return map[string]any{"type": "response.function_call_arguments.delta", "item_id": id, "delta": "{"}
	}
	testutil.False(t, filter.Drop(added("fc_custom", "exec")), "the item itself is raised, not dropped")
	testutil.False(t, filter.Drop(added("fc_plain", "lookup")), "a plain call is kept")
	testutil.True(t, filter.Drop(delta("fc_custom")), "a lowered call's argument delta is dropped")
	testutil.True(t, filter.Drop(map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc_custom"}), "and its done event")
	testutil.False(t, filter.Drop(delta("fc_plain")), "a plain call's delta is kept")
	testutil.False(t, (*LoweredCallFilter)(nil).Drop(delta("fc_custom")), "a nil filter drops nothing")
	testutil.True(t, strings.Contains(customToolDescription(map[string]any{"description": "d"}), "input string field"), "the input contract is stated")
}
