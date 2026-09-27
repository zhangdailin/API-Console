package handler

import (
	"bytes"
	"github.com/goccy/go-json"
)

// StopSequences accepts both OpenAI's string and array spellings of stop.
type StopSequences []string

func (s *StopSequences) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		*s = []string{value}
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	*s = values
	return nil
}

func (r ClaudeRequest) outputTokenLimit() *int {
	if r.MaxCompletionTokens != nil {
		return r.MaxCompletionTokens
	}
	return r.MaxTokens
}

func (r ClaudeRequest) stopSequences() []string {
	if r.StopSequences != nil {
		return append([]string{}, r.StopSequences...)
	}
	if r.Stop != nil {
		return append([]string{}, r.Stop...)
	}
	return nil
}
