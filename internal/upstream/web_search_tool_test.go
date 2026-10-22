package upstream

import (
	"testing"

	"orchids-api/internal/testutil"
)

// TestServerWebSearchToolIsRefused: Claude Code's WebSearch declares the
// Anthropic server tool and expects the API to run the search. These channels
// cannot, so the request is refused with that reason instead of failing
// silently.
func TestServerWebSearchToolIsRefused(t *testing.T) {
	req := UpstreamRequest{Tools: []any{map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 8}}}
	err := req.ValidateProtocolControls("qoder")
	testutil.Error(t, err)
	testutil.MustContain(t, err.Error(), "qoder does not support web search")
	testutil.MustContain(t, err.Error(), "web_search_20250305")
	testutil.MustContain(t, err.Error(), "/grok")
}

// TestClientFunctionNamedWebSearchIsAllowed: a client's own function that is
// merely called web_search is an ordinary tool.
func TestClientFunctionNamedWebSearchIsAllowed(t *testing.T) {
	for name, tool := range map[string]map[string]any{
		"anthropic custom tool": {"name": "web_search", "input_schema": map[string]any{"type": "object"}},
		"explicit custom type":  {"type": "custom", "name": "web_search", "input_schema": map[string]any{"type": "object"}},
		"openai function":       {"type": "function", "function": map[string]any{"name": "web_search"}},
	} {
		t.Run(name, func(t *testing.T) {
			testutil.NoError(t, UpstreamRequest{Tools: []any{tool}}.ValidateProtocolControls("qoder"))
		})
	}
}
