package handler

import (
	"strings"

	"encoding/json"

	"orchids-api/internal/tiktoken"
	"orchids-api/internal/toolname"
)

func declaredToolNames(tools []interface{}) []string {
	if len(tools) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(tools)*2)
	out := make([]string, 0, len(tools)*2)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := name
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, name)
	}

	for _, tool := range tools {
		name, _, _ := toolname.ExtractToolSpecFields(tool)
		if name == "" {
			continue
		}
		add(name)
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

// estimateToolsTokens reports how many tokens the tool definitions contribute.
//
// It measures the tools exactly as they are forwarded. The previous version
// measured a compacted projection of them — an allowlist of at most 24 tools,
// every description cut to 128 characters and every schema to 4 KiB — which
// described a request this gateway never sends. The number is what a client
// budgets against (directly through /v1/messages/count_tokens), so under-
// reporting it let a client believe it had room it did not have.
func estimateToolsTokens(tools []interface{}) int {
	if len(tools) == 0 {
		return 0
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		return 0
	}
	var estimator tiktoken.Estimator
	estimator.AddBytes(raw)
	return estimator.Count()
}
