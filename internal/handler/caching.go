package handler

import (
	"strings"

	"orchids-api/internal/prompt"
)

// applyCacheStrategy uses at most four new breakpoints: system, tools, the
// previous user turn, and the last user turn. Existing markers don't consume
// the budget. Mixed picks the largest final block (last wins ties); other
// enabled strategies pick the final block, preserving the legacy split policy.
func applyCacheStrategy(req *ClaudeRequest, strategy string) {
	strategy = strings.ToLower(strings.TrimSpace(strategy))
	if strategy == "mix" {
		strategy = "mixed"
	}
	if strategy == "" || strategy == "none" || strategy == "off" {
		return
	}
	breakpoints := 0
	addCache := func(control **prompt.CacheControl) {
		if breakpoints < 4 && *control == nil {
			*control = &prompt.CacheControl{Type: "ephemeral"}
			breakpoints++
		}
	}
	if len(req.System) > 0 {
		addCache(&req.System[len(req.System)-1].CacheControl)
	}
	if len(req.Tools) > 0 && breakpoints < 4 {
		if tool, ok := req.Tools[len(req.Tools)-1].(map[string]any); ok {
			if _, exists := tool["cache_control"]; !exists {
				tool["cache_control"] = map[string]string{"type": "ephemeral"}
				breakpoints++
			}
		}
	}

	previous, last := -1, -1
	for i := range req.Messages {
		if req.Messages[i].Role == "user" {
			previous, last = last, i
		}
	}
	for _, index := range []int{previous, last} {
		if index < 0 || req.Messages[index].Content.IsString() {
			continue
		}
		blocks := req.Messages[index].Content.GetBlocks()
		if len(blocks) == 0 {
			continue
		}
		selected := len(blocks) - 1
		if index == last && strategy == "mixed" {
			maxSize := 0
			for i := range blocks {
				size := len(blocks[i].Text)
				if blocks[i].Type == "image" && blocks[i].Source != nil {
					size = len(blocks[i].Source.Data)
				}
				if size >= maxSize {
					maxSize, selected = size, i
				}
			}
		}
		addCache(&blocks[selected].CacheControl)
		req.Messages[index].Content.Blocks = blocks
	}
}
