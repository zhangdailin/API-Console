package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"orchids-api/internal/adapter"
	"orchids-api/internal/debug"
	"orchids-api/internal/tiktoken"
	"orchids-api/internal/util"

	"github.com/kballard/go-shellquote"
)

// policyCommandLine matches the "Command: <line>" line of a policy spec.
//
// It is compiled once at package scope. As a call-local regexp.MustCompile it
// recompiled the pattern on every request that carried a policy spec — the
// compiler's program cache only helps regexp.Compile, and MustCompile still paid
// the whole parse-and-compile path on each call.
var policyCommandLine = regexp.MustCompile(`(?m)^Command:\s*(.+)$`)

func isCommandPrefixRequest(req ClaudeRequest) (bool, string) {
	userText := extractUserText(req.Messages)
	if userText == "" {
		return false, ""
	}
	lower := strings.ToLower(userText)
	if !strings.Contains(lower, "<policy_spec>") && !strings.Contains(lower, "command prefix") {
		return false, ""
	}
	command := extractCommandFromPolicy(userText)
	if command == "" {
		return false, ""
	}
	return true, command
}

func extractCommandFromPolicy(text string) string {
	if match := policyCommandLine.FindStringSubmatch(text); len(match) > 1 {
		return strings.TrimSpace(match[1])
	}
	if _, after, ok := strings.Cut(text, "Command:"); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// Local responses use a cold JSON encoder, not the upstream SSE hot path. One
// schema keeps start/content/stop frames consistent while omitting empty deltas.
type localOpenAIDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type localOpenAIChoice struct {
	Index        int              `json:"index"`
	Delta        localOpenAIDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason,omitempty"`
}

type localOpenAIChunk struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []localOpenAIChoice `json:"choices"`
}

func newLocalOpenAIChunk(id, model string, delta localOpenAIDelta, finish *string) localOpenAIChunk {
	return localOpenAIChunk{
		ID: id, Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: model,
		Choices: []localOpenAIChoice{{Delta: delta, FinishReason: finish}},
	}
}

func writeLocalTextResponse(w http.ResponseWriter, req ClaudeRequest, responseFormat adapter.ResponseFormat, text string, startTime time.Time, logger *debug.Logger) {
	inputTokens := tiktoken.EstimateTextTokens(extractUserText(req.Messages))
	outputTokens := tiktoken.EstimateTextTokens(text)
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixMilli())

	if req.Stream {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}

		if responseFormat == adapter.FormatOpenAI {
			w.Header().Set("Content-Type", "text/event-stream")

			startChunk := newLocalOpenAIChunk(msgID, req.Model, localOpenAIDelta{Role: "assistant"}, nil)
			stopReason := "stop"
			stopChunk := newLocalOpenAIChunk(msgID, req.Model, localOpenAIDelta{}, &stopReason)
			rawStart, _ := json.Marshal(startChunk)
			_ = writeOpenAIFrame(w, rawStart)
			if logger != nil {
				logger.LogOutputSSE("message_start", string(rawStart))
			}
			if text != "" {
				contentChunk := newLocalOpenAIChunk(msgID, req.Model, localOpenAIDelta{Content: text}, nil)
				rawContent, _ := json.Marshal(contentChunk)
				_ = writeOpenAIFrame(w, rawContent)
				if logger != nil {
					logger.LogOutputSSE("content_block_delta", string(rawContent))
				}
			}
			rawStop, _ := json.Marshal(stopChunk)
			_ = writeOpenAIFrame(w, rawStop)
			_, _ = w.Write(sseDoneLineBytes)
			flusher.Flush()
			if logger != nil {
				logger.LogOutputSSE("message_stop", string(rawStop))
				logger.LogSummary(inputTokens, outputTokens, time.Since(startTime), "end_turn")
			}
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		write := func(event string, data []byte) {
			_ = writeSSEFrameBytes(w, event, data)
			flusher.Flush()
			if logger != nil {
				logger.LogOutputSSE(event, string(data))
			}
		}

		startData, _ := marshalSSEMessageStartBytes(msgID, req.Model, inputTokens, 0)
		blockStart, _ := marshalSSEContentBlockStartTextBytes(0)
		blockDelta, _ := marshalSSEContentBlockDeltaTextBytes(0, text)
		blockStop, _ := marshalSSEContentBlockStopBytes(0)
		msgDelta, _ := marshalSSEMessageDeltaBytes("end_turn", outputTokens)
		write("message_start", startData)
		write("content_block_start", blockStart)
		write("content_block_delta", blockDelta)
		write("content_block_stop", blockStop)
		write("message_delta", msgDelta)
		write("message_stop", sseMessageStopBytes)
		if logger != nil {
			logger.LogSummary(inputTokens, outputTokens, time.Since(startTime), "end_turn")
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if responseFormat == adapter.FormatOpenAI {
		stopReason := "stop"
		resp := openAINonStreamResponse{
			ID:      msgID,
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   req.Model,
			Choices: []openAINonStreamChoice{{
				Index: 0,
				Message: openAINonStreamMessage{
					Role:    "assistant",
					Content: text,
				},
				FinishReason: &stopReason,
			}},
			Usage: openAINonStreamUsage{
				PromptTokens:     inputTokens,
				CompletionTokens: outputTokens,
				TotalTokens:      inputTokens + outputTokens,
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil && logger != nil {
			logger.LogOutputSSE("error", fmt.Sprintf("failed to encode response: %v", err))
		}
		if logger != nil {
			logger.LogSummary(inputTokens, outputTokens, time.Since(startTime), "end_turn")
		}
		return
	}

	response := struct {
		ID           string              `json:"id"`
		Type         string              `json:"type"`
		Role         string              `json:"role"`
		Content      []map[string]string `json:"content"`
		Model        string              `json:"model"`
		StopReason   string              `json:"stop_reason"`
		StopSequence interface{}         `json:"stop_sequence"`
		Usage        struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}{
		ID:           msgID,
		Type:         "message",
		Role:         "assistant",
		Content:      []map[string]string{{"type": "text", "text": text}},
		Model:        req.Model,
		StopReason:   "end_turn",
		StopSequence: nil,
		Usage: struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		}{InputTokens: inputTokens, OutputTokens: outputTokens},
	}
	if err := json.NewEncoder(w).Encode(response); err != nil && logger != nil {
		logger.LogOutputSSE("error", fmt.Sprintf("failed to encode response: %v", err))
	}
	if logger != nil {
		logger.LogSummary(inputTokens, outputTokens, time.Since(startTime), "end_turn")
	}
}

func writeCommandPrefixResponse(w http.ResponseWriter, req ClaudeRequest, responseFormat adapter.ResponseFormat, prefix string, startTime time.Time, logger *debug.Logger) {
	prefix = strings.TrimSpace(prefix)
	prefix = util.FirstNonEmptyUntrimmed(prefix, "none")
	writeLocalTextResponse(w, req, responseFormat, prefix, startTime, logger)
}

func writeTopicClassifierResponse(w http.ResponseWriter, req ClaudeRequest, responseFormat adapter.ResponseFormat, startTime time.Time, logger *debug.Logger) {
	isNewTopic, title := classifyTopicRequest(req)
	payload := map[string]interface{}{
		"isNewTopic": isNewTopic,
		"title":      nil,
	}
	if isNewTopic {
		payload["title"] = title
	}
	raw, _ := json.Marshal(payload)
	writeLocalTextResponse(w, req, responseFormat, string(raw), startTime, logger)
}

func writeTitleGenerationResponse(w http.ResponseWriter, req ClaudeRequest, responseFormat adapter.ResponseFormat, startTime time.Time, logger *debug.Logger) {
	raw, _ := json.Marshal(map[string]string{
		"title": generateTopicTitle(extractUserText(req.Messages)),
	})
	writeLocalTextResponse(w, req, responseFormat, string(raw), startTime, logger)
}

func writeSuggestionModeResponse(w http.ResponseWriter, req ClaudeRequest, responseFormat adapter.ResponseFormat, startTime time.Time, logger *debug.Logger) {
	writeLocalTextResponse(w, req, responseFormat, buildLocalSuggestion(req.Messages), startTime, logger)
}

func detectCommandPrefix(command string) string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return "none"
	}
	if looksLikeCommandInjection(trimmed) {
		return "command_injection_detected"
	}
	tokens, err := shellquote.Split(trimmed)
	if err != nil || len(tokens) == 0 {
		return "command_injection_detected"
	}

	var prefix []string
	i := 0
	for i < len(tokens) && isEnvAssignment(tokens[i]) {
		prefix = append(prefix, tokens[i])
		i++
	}
	if i >= len(tokens) {
		return "none"
	}

	cmdIndex := i
	cmd := tokens[i]
	i++
	lowerCmd := strings.ToLower(cmd)

	switch lowerCmd {
	case "git":
		subIdx := findGitSubcommandIndex(tokens, i)
		if subIdx == -1 {
			return "none"
		}
		sub := strings.ToLower(tokens[subIdx])
		if sub == "push" && subIdx == len(tokens)-1 {
			return "none"
		}
		prefix = append(prefix, tokens[cmdIndex:subIdx+1]...)
		return strings.Join(prefix, " ")
	case "go", "gg", "potion", "pig":
		if i >= len(tokens) {
			return "none"
		}
		prefix = append(prefix, tokens[cmdIndex:i+1]...)
		return strings.Join(prefix, " ")
	case "npm":
		if i >= len(tokens) {
			return "none"
		}
		sub := strings.ToLower(tokens[i])
		switch sub {
		case "run":
			if i+1 >= len(tokens) {
				return "none"
			}
			if i+2 >= len(tokens) && len(prefix) == 0 {
				return "none"
			}
			prefix = append(prefix, tokens[cmdIndex:i+2]...)
			return strings.Join(prefix, " ")
		case "test":
			if i+1 >= len(tokens) {
				return "none"
			}
			prefix = append(prefix, tokens[cmdIndex:i+1]...)
			return strings.Join(prefix, " ")
		default:
			prefix = append(prefix, tokens[cmdIndex])
			return strings.Join(prefix, " ")
		}
	default:
		prefix = append(prefix, tokens[cmdIndex])
		return strings.Join(prefix, " ")
	}
}

func isEnvAssignment(token string) bool {
	return !(token == "" || !envAssignPattern.MatchString(token))
}

var envAssignPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

func looksLikeCommandInjection(command string) bool {
	if strings.Contains(command, "\n") || strings.Contains(command, "\r") || strings.Contains(command, "`") || strings.Contains(command, "$(") {
		return true
	}
	if strings.Contains(command, ";") || strings.Contains(command, "||") || strings.Contains(command, "&&") {
		return true
	}
	return strings.Contains(command, "|")
}

func findGitSubcommandIndex(tokens []string, start int) int {
	for i := start; i < len(tokens); i++ {
		if gitSubcommands[strings.ToLower(tokens[i])] {
			return i
		}
	}
	return -1
}

var gitSubcommands = map[string]bool{
	"add":      true,
	"branch":   true,
	"checkout": true,
	"clone":    true,
	"commit":   true,
	"diff":     true,
	"fetch":    true,
	"log":      true,
	"merge":    true,
	"pull":     true,
	"push":     true,
	"rebase":   true,
	"reset":    true,
	"show":     true,
	"stash":    true,
	"status":   true,
	"tag":      true,
	"remote":   true,
}
