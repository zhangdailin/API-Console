package warp

import (
	"fmt"
	"unicode/utf8"

	"orchids-api/internal/prompt"
)

// validateThinkingHistory bounds all historical thinking and signatures before
// credential or upstream work. The signature belongs to its original provider;
// it is not verified, reused for auth, logged, or converted into a Warp prompt.
func validateThinkingHistory(messages []prompt.Message) error {
	total := 0
	for i, message := range messages {
		for j, block := range message.Content.Blocks {
			if block.Type != "thinking" {
				continue
			}
			path := fmt.Sprintf("messages[%d].content[%d]", i, j)
			if !validMessageBlock(block, message.Role) {
				return requestFieldError("invalid_thinking_history", path)
			}
			total += len(block.Thinking) + len(block.Signature)
			if total > thinkingHistoryLimit {
				return requestBytesError("thinking_history_total_bytes", "messages", total, thinkingHistoryLimit)
			}
		}
	}
	return nil
}

func validThinkingHistory(block prompt.ContentBlock, role string) bool {
	return role == "assistant" && utf8.ValidString(block.Thinking) && utf8.ValidString(block.Signature) && len(block.Thinking) <= thinkingHistoryLimit && len(block.Signature) <= thinkingHistoryLimit-len(block.Thinking)
}
