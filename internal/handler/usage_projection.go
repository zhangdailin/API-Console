package handler

import (
	"encoding/json"
	"strconv"
)

// MarshalJSON does not publish local estimates as authoritative counters when an
// explicitly disjoint provider usage report omitted its input components.
func (u openAINonStreamUsage) MarshalJSON() ([]byte, error) {
	type plain openAINonStreamUsage
	if !u.Partial {
		return json.Marshal(plain(u))
	}
	out := map[string]any{}
	if u.KnownPrompt {
		out["prompt_tokens"] = u.PromptTokens
	}
	if u.KnownOutput {
		out["completion_tokens"] = u.CompletionTokens
	}
	if u.KnownPrompt && u.KnownOutput {
		out["total_tokens"] = u.TotalTokens
	}
	if u.PromptTokensDetails != nil {
		out["prompt_tokens_details"] = u.PromptTokensDetails
	}
	if u.CacheCreationInputTokens != nil {
		out["cache_creation_input_tokens"] = *u.CacheCreationInputTokens
	}
	return json.Marshal(out)
}

type openAINonStreamUsage struct {
	PromptTokens             int            `json:"prompt_tokens"`
	CompletionTokens         int            `json:"completion_tokens"`
	TotalTokens              int            `json:"total_tokens"`
	PromptTokensDetails      map[string]int `json:"prompt_tokens_details,omitempty"`
	CacheCreationInputTokens *int           `json:"cache_creation_input_tokens,omitempty"`
	Partial                  bool           `json:"-"`
	KnownPrompt              bool           `json:"-"`
	KnownOutput              bool           `json:"-"`
}

func appendCanonicalWireUsage(dst []byte, input, output, cached, write int) []byte {
	dst = append(dst, `{"prompt_tokens":`...)
	dst = strconv.AppendInt(dst, int64(input), 10)
	dst = append(dst, `,"completion_tokens":`...)
	dst = strconv.AppendInt(dst, int64(output), 10)
	dst = append(dst, `,"cache_read_input_tokens":`...)
	dst = strconv.AppendInt(dst, int64(cached), 10)
	dst = append(dst, `,"cache_creation_input_tokens":`...)
	dst = strconv.AppendInt(dst, int64(write), 10)
	return append(dst, '}')
}
