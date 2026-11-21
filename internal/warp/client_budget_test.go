package warp

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func clientBudgetRequest() upstream.UpstreamRequest {
	return upstream.UpstreamRequest{Model: "model-a", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}}}
}

func clientBudgetSchema(size int) map[string]any {
	return map[string]any{"description": strings.Repeat("s", size-len(`{"description":""}`))}
}

func clientBudgetTool(name string, schema map[string]any, description string, openAI bool) map[string]any {
	if openAI {
		return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": description, "parameters": schema, "strict": false}}
	}
	return map[string]any{"name": name, "description": description, "input_schema": schema}
}

func clientBudgetDefinitionsBytes(t *testing.T, tools []any) int {
	t.Helper()
	total := 0
	for _, tool := range tools {
		encoded, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		total += len(encoded)
	}
	return total
}

func assertClientBudgetError(t *testing.T, err error, message string) {
	t.Helper()
	var app *apperrors.AppError
	if !errors.As(err, &app) || app.HTTPStatus != 400 || app.Code != "invalid_request_error" || app.Message != message {
		t.Fatalf("unexpected error: got %v, want %q", err, message)
	}
}

func TestClientBudgetRealShapedInstructionsAndTools(t *testing.T) {
	req := clientBudgetRequest()
	cache := &prompt.CacheControl{Type: "ephemeral"}
	req.System = []prompt.SystemItem{
		{Type: "text", Text: "  " + strings.Repeat("a", 3694), CacheControl: cache},
		{Type: "text", Text: strings.Repeat("b", 3697), CacheControl: cache},
		{Type: "text", Text: strings.Repeat("c", 3695) + "\n ", CacheControl: cache},
	}
	trailing := " \n" + strings.Repeat("d", 13229) + "\n "
	user := strings.Repeat("u", 581)
	req.Messages = []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "text", Text: user, CacheControl: cache}}}},
		{Role: "system", Content: prompt.MessageContent{Text: trailing}},
	}
	// Exactly 29 definitions, 48 KiB of schemas, a 20 KiB description, and
	// 81 KiB of complete JSON definitions. Every byte is synthetic.
	for i := 0; i < 29; i++ {
		schemaBytes := (48 << 10) / 29
		if i == 28 {
			schemaBytes += (48 << 10) % 29
		}
		description := ""
		if i == 0 {
			description = strings.Repeat("x", 20<<10)
		}
		req.Tools = append(req.Tools, clientBudgetTool(fmt.Sprintf("tool_%02d", i), clientBudgetSchema(schemaBytes), description, false))
	}
	req.Tools[1].(map[string]any)["description"] = strings.Repeat("y", (81<<10)-clientBudgetDefinitionsBytes(t, req.Tools))
	if got := clientBudgetDefinitionsBytes(t, req.Tools); got != 81<<10 {
		t.Fatalf("fixture definition bytes = %d", got)
	}
	before, _ := json.Marshal(req)
	prepared, err := validateRequest(req)
	if err != nil {
		t.Fatalf("real-shaped request rejected: %v", err)
	}
	instructions := strings.Join([]string{req.System[0].Text, req.System[1].Text, req.System[2].Text, trailing}, "\n")
	if prepared.Query != "System instructions:\n"+instructions+"\n\nUser: "+user || prepared.CurrentText != user {
		t.Fatal("instruction bytes, order, or current user text changed")
	}
	if len(prepared.Tools) != 29 || prepared.Tools[0].Description != req.Tools[0].(map[string]any)["description"] {
		t.Fatal("tool definitions were lost or truncated")
	}
	for i, tool := range prepared.Tools {
		if !reflect.DeepEqual(tool.Schema, req.Tools[i].(map[string]any)["input_schema"]) {
			t.Fatalf("schema %d was rewritten", i)
		}
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("request was mutated while ignoring cache hints")
	}
	if _, err := buildRequest(req.Model, prepared); err != nil {
		t.Fatalf("accepted request cannot be encoded: %v", err)
	}
}

func TestClientBudgetSystemNormalizationPreservesOrder(t *testing.T) {
	req := clientBudgetRequest()
	req.System = []prompt.SystemItem{{Type: "text", Text: " top "}}
	req.Messages = []prompt.Message{
		{Role: "system", Content: prompt.MessageContent{Text: "before"}},
		{Role: "user", Content: prompt.MessageContent{Text: "old"}},
		{Role: "system", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "text", Text: " middle ", CacheControl: &prompt.CacheControl{Type: "ephemeral"}}, {Type: "text", Text: "\nnext"}}}},
		{Role: "assistant", Content: prompt.MessageContent{Text: "answer"}},
		{Role: "user", Content: prompt.MessageContent{Text: "current"}},
		{Role: "system", Content: prompt.MessageContent{Text: "after\n "}},
	}
	prepared, err := validateRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	want := "System instructions:\n top \nbefore\n middle \nnext\nafter\n \n\nUser: old\n\nAssistant: answer\n\nUser: current"
	if prepared.Query != want {
		t.Fatalf("incorrect normalization: %q", prepared.Query)
	}
	// Removing instructions must not repair assistant-first or unfinished dialog.
	for _, roles := range [][]string{{"assistant", "system", "user"}, {"user", "system", "assistant"}, {"system"}} {
		req.Messages = nil
		for _, role := range roles {
			req.Messages = append(req.Messages, prompt.Message{Role: role, Content: prompt.MessageContent{Text: "x"}})
		}
		if _, err := validateRequest(req); err == nil {
			t.Fatalf("invalid dialog accepted: %v", roles)
		}
	}
	// Instructions do not consume the bounded user/assistant turn count.
	req.Messages = nil
	for i := 0; i < 8; i++ {
		req.Messages = append(req.Messages, prompt.Message{Role: "user", Content: prompt.MessageContent{Text: "q"}}, prompt.Message{Role: "system", Content: prompt.MessageContent{Text: "pinned"}}, prompt.Message{Role: "assistant", Content: prompt.MessageContent{Text: "a"}})
	}
	req.Messages = append(req.Messages, prompt.Message{Role: "user", Content: prompt.MessageContent{Text: "now"}}, prompt.Message{Role: "system", Content: prompt.MessageContent{Text: "last"}})
	if _, err := validateRequest(req); err != nil {
		t.Fatalf("eight dialog turns plus instructions rejected: %v", err)
	}
}

func TestClientBudgetSystemNoLongerCapped(t *testing.T) {
	for _, size := range []int{32769, 63712, 128 << 10, 256 << 10} {
		req := clientBudgetRequest()
		text := strings.Repeat("s", size)
		req.System = []prompt.SystemItem{{Type: "text", Text: text}}
		req.Messages = append(req.Messages, prompt.Message{Role: "system", Content: prompt.MessageContent{Text: "trailing"}})
		got, err := validateRequest(req)
		if err != nil || !strings.HasPrefix(got.Query, "System instructions:\n"+text+"\ntrailing\n\n") {
			t.Fatalf("large instructions lost: %v", err)
		}
	}
	req := clientBudgetRequest()
	req.Messages[0].Content.Text = strings.Repeat("u", 4096)
	if _, err := validateRequest(req); err != nil {
		t.Fatal(err)
	}
	req.Messages[0].Content.Text += "u"
	if _, err := validateRequest(req); err != nil {
		t.Fatal("user above former 4096-byte cap rejected")
	}
}

func TestClientBudgetToolDescriptionAndSchemaBoundaries(t *testing.T) {
	for _, openAI := range []bool{false, true} {
		t.Run(fmt.Sprintf("openAI=%t", openAI), func(t *testing.T) {
			tool := clientBudgetTool("lookup", map[string]any{}, strings.Repeat("d", (32<<10)), openAI)
			if _, err := convertToolsWithError([]any{tool}); err != nil {
				t.Fatal(err)
			}
			fn := tool
			if openAI {
				fn = tool["function"].(map[string]any)
			}
			fn["description"] = strings.Repeat("d", (32<<10)+1)
			converted, err := convertToolsWithError([]any{tool})
			if err != nil || len(converted) != 1 || converted[0].Description != fn["description"] {
				t.Fatal("description above former 32 KiB cap rejected or truncated")
			}
			tool = clientBudgetTool("lookup", clientBudgetSchema((64 << 10)), "", openAI)
			if _, err := convertToolsWithError([]any{tool}); err != nil {
				t.Fatal(err)
			}
			tool = clientBudgetTool("lookup", clientBudgetSchema((64<<10)+1), "", openAI)
			converted, err = convertToolsWithError([]any{tool})
			if err != nil || len(converted) != 1 || !reflect.DeepEqual(converted[0].Schema, clientBudgetSchema((64<<10)+1)) {
				t.Fatal("schema above former 64 KiB cap rejected or truncated")
			}
		})
	}
	tools := []any{clientBudgetTool("first", clientBudgetSchema((64<<10)/2), "", false), clientBudgetTool("second", clientBudgetSchema((64<<10)/2), "", false)}
	if _, err := convertToolsWithError(tools); err != nil {
		t.Fatal(err)
	}
	tools[1].(map[string]any)["input_schema"] = clientBudgetSchema((64<<10)/2 + 1)
	if _, err := convertToolsWithError(tools); err != nil {
		t.Fatalf("removed aggregate schema limit rejected valid individual schemas: %v", err)
	}
	for _, openAI := range []bool{false, true} {
		tools = nil
		for i := 0; i < 48; i++ {
			tools = append(tools, clientBudgetTool(fmt.Sprintf("schema_%d", i), clientBudgetSchema(2048), "", openAI))
		}
		converted, err := convertToolsWithError(tools)
		if err != nil || len(converted) != 48 {
			t.Fatalf("aggregate schemas truncated/rejected: %v", err)
		}
		for _, tool := range converted {
			encoded, err := json.Marshal(tool.Schema)
			if err != nil || len(encoded) != 2048 {
				t.Fatal("schema changed by removed cumulative budget")
			}
		}
	}
}

// The old cumulative full-definition limit was removed explicitly. Per-tool
// and schema bounds remain independent and definitions must not be truncated.
func TestClientBudgetFullToolDefinitionsMayExceedFormerLimit(t *testing.T) {
	for _, openAI := range []bool{false, true} {
		t.Run(fmt.Sprintf("openAI=%t", openAI), func(t *testing.T) {
			for _, target := range []int{163841, 164060, 512 << 10} {
				var tools []any
				count := 17
				for i := 0; i < count; i++ {
					tools = append(tools, clientBudgetTool(fmt.Sprintf("tool_%d", i), map[string]any{}, "", openAI))
				}
				missing := target - clientBudgetDefinitionsBytes(t, tools)
				for _, entry := range tools {
					fn := entry.(map[string]any)
					if openAI {
						fn = fn["function"].(map[string]any)
					}
					n := min(missing, (32 << 10))
					fn["description"] = strings.Repeat("d", n)
					missing -= n
				}
				if missing != 0 || clientBudgetDefinitionsBytes(t, tools) != target {
					t.Fatal("invalid over-former-limit fixture")
				}
				got, err := convertToolsWithError(tools)
				if err != nil || len(got) != len(tools) {
					t.Fatalf("definition total %d rejected: %v", target, err)
				}
				for i, entry := range tools {
					fn := entry.(map[string]any)
					if openAI {
						fn = fn["function"].(map[string]any)
					}
					if got[i].Description != fn["description"] {
						t.Fatal("tool description truncated")
					}
				}
			}
		})
	}
	tools := make([]any, 6)
	for i := range tools {
		tools[i] = clientBudgetTool(fmt.Sprintf("escaped_%d", i), map[string]any{}, strings.Repeat("<", 5000), false)
	}
	if clientBudgetDefinitionsBytes(t, tools) <= 160<<10 {
		t.Fatal("escaping fixture does not cross former limit")
	}
	if _, err := convertToolsWithError(tools); err != nil {
		t.Fatalf("escaped definitions rejected by removed limit: %v", err)
	}
}

func TestClientBudgetToolCountNoLongerCappedAndDuplicateErrors(t *testing.T) {
	for _, openAI := range []bool{false, true} {
		for _, count := range []int{65, 78, 128, 256} {
			var tools []any
			for i := 0; i < count; i++ {
				tools = append(tools, clientBudgetTool(fmt.Sprintf("tool_%d", i), map[string]any{}, fmt.Sprintf("description_%d", i), openAI))
			}
			req := clientBudgetRequest()
			req.Tools = tools
			got, err := validateRequest(req)
			if err != nil || len(got.Tools) != count {
				t.Fatalf("count %d rejected/truncated: %v", count, err)
			}
			for i, tool := range got.Tools {
				if tool.Name != fmt.Sprintf("tool_%d", i) || tool.Description != fmt.Sprintf("description_%d", i) {
					t.Fatal("tool order or description changed")
				}
			}
			fn := tools[count-1].(map[string]any)
			if openAI {
				fn = fn["function"].(map[string]any)
			}
			fn["name"] = "TOOL_0"
			if _, err := validateRequest(req); err == nil {
				t.Fatal("duplicate name beyond former64 limit accepted")
			}
		}
	}
	for _, names := range [][2]string{{"private-name", "PRIVATE-NAME"}, {"K", "K"}, {"S", "ſ"}} {
		_, err := convertToolsWithError([]any{clientBudgetTool(names[0], map[string]any{}, "", false), clientBudgetTool(names[1], map[string]any{}, "", true)})
		assertClientBudgetError(t, err, "Warp duplicate_tool_name at tools[1].function.name")
	}
}

func TestClientBudgetRejectsInvalidCacheAndUnsupportedBlocks(t *testing.T) {
	for _, block := range []prompt.ContentBlock{
		{Type: "text", Text: "instructions", CacheControl: &prompt.CacheControl{Type: "permanent"}},
		{Type: "text", Text: "instructions", CacheControl: &prompt.CacheControl{}},
		{Type: "text", Text: "instructions", URL: "https://example.invalid"},
		{Type: "image", Source: &prompt.ImageSource{Type: "base64", Data: "synthetic"}},
		{Type: "unknown", Text: "instructions"},
	} {
		for _, role := range []string{"system", "user"} {
			req := clientBudgetRequest()
			message := prompt.Message{Role: role, Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{block}}}
			if role == "system" {
				req.Messages = append(req.Messages, message)
			} else {
				req.Messages[0] = message
			}
			if _, err := validateRequest(req); err == nil {
				t.Fatalf("unsupported %s block accepted: %s", role, block.Type)
			}
		}
	}
	req := clientBudgetRequest()
	req.System = []prompt.SystemItem{{Type: "text", Text: "private-instructions", CacheControl: &prompt.CacheControl{Type: "private-invalid-marker"}}}
	_, err := validateRequest(req)
	assertClientBudgetError(t, err, "Warp invalid_cache_control at system[0].cache_control")
	for _, item := range []prompt.SystemItem{{Type: "image", Text: "hidden"}, {Type: "text", Text: string([]byte{0xff})}} {
		req.System = []prompt.SystemItem{item}
		if _, err := validateRequest(req); err == nil {
			t.Fatal("invalid separate system item accepted")
		}
	}
}

func convertTools(raw []any) ([]mcpTool, bool) {
	tools, err := convertToolsWithError(raw)
	return tools, err == nil
}
