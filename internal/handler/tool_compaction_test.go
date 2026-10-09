package handler

import (
	"orchids-api/internal/testutil"
	"strings"
	"testing"
)

func sampleIncomingTools() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "Write",
				"description": "write file content safely",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file_path": map[string]interface{}{"type": "string"},
						"content":   map[string]interface{}{"type": "string", "description": "utf-8 内容"},
					},
				},
			},
		},
		map[string]interface{}{
			"name":        "Read",
			"description": "read file content",
			"input_schema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"file_path": map[string]interface{}{"type": "string"}},
			},
		},
	}
}

// The estimate is what a client budgets against, so it has to describe the
// request this gateway actually sends. The previous implementation measured a
// compacted projection (at most 24 tools, 128-character descriptions, 4 KiB
// schemas) that was never forwarded, which under-reported the input.
func TestEstimateToolsTokensMeasuresTheToolsThatAreSent(t *testing.T) {
	tools := sampleIncomingTools()
	got := estimateToolsTokens(tools)
	testutil.Falsef(t, got <= 0, "estimateToolsTokens() = %d, want a positive estimate", got)
	// A tool the old projection would have dropped must still change the
	// estimate, otherwise the number does not describe the request.
	withExtra := append(append([]interface{}{}, tools...), map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "Agent",
			"description": strings.Repeat("long description ", 200),
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"prompt": map[string]interface{}{"type": "string", "description": strings.Repeat("payload ", 200)},
				},
			},
		},
	})
	testutil.False(t, estimateToolsTokens(withExtra) <= estimateToolsTokens(tools), "a tool the projection would have dropped did not change the estimate")
}

func TestEstimateToolsTokensIsEmptyForNoTools(t *testing.T) {
	testutil.Equal(t, estimateToolsTokens(nil), 0)
	testutil.Equal(t, estimateToolsTokens([]interface{}{}), 0)
}

// A tool description the old code cut to 128 characters still counts in full.
func TestEstimateToolsTokensCountsLongDescriptionsInFull(t *testing.T) {
	short := []interface{}{map[string]interface{}{
		"name":        "Bash",
		"description": "run a command",
	}}
	long := []interface{}{map[string]interface{}{
		"name":        "Bash",
		"description": strings.Repeat("run a command with a great deal of extra guidance ", 40),
	}}
	testutil.False(t, estimateToolsTokens(long) <= estimateToolsTokens(short), "a long description must count in full, not be capped at 128 characters")
}

func BenchmarkEstimateToolsTokens(b *testing.B) {
	tools := sampleIncomingTools()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = estimateToolsTokens(tools)
	}
}

func TestDeclaredToolNames_EmptyAndNoValidDeclarations(t *testing.T) {
	for _, tools := range [][]interface{}{
		nil,
		{},
		{map[string]interface{}{"name": "  "}, map[string]interface{}{"description": "missing name"}},
	} {
		got := declaredToolNames(tools)
		testutil.Falsef(t, got != nil, "declaredToolNames(%#v) = %#v, want nil", tools, got)
	}
}

func TestDeclaredToolNames_PreservesExactNames(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{"name": " Agent "},
		map[string]interface{}{"name": "task"},
		map[string]interface{}{"name": "WEB_SEARCH"},
		map[string]interface{}{"name": "web_search"},
	}
	got := declaredToolNames(tools)
	want := []string{"Agent", "task", "WEB_SEARCH", "web_search"}
	testutil.Equal(t, len(got), len(want))
	for i := range want {
		testutil.Equal(t, got[i], want[i])
	}
}

func TestDeclaredToolNames_KeepCustomAndCanonicalAliases(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{"name": "workspace_search"},
		map[string]interface{}{"name": "read_files"},
		map[string]interface{}{"name": "Read"},
	}

	got := declaredToolNames(tools)
	want := []string{"workspace_search", "read_files", "Read"}
	testutil.Equal(t, len(got), len(want))
	for i := range want {
		testutil.Equal(t, got[i], want[i])
	}
}
