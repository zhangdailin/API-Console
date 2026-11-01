package handler

import (
	"strings"
	"testing"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/testutil"
	"orchids-api/internal/tiktoken"
	"orchids-api/internal/upstream"
)

func TestStreamHandler_TokensUsed_OverridesEstimation(t *testing.T) {
	sh, _ := newStreamTestHandler(t, false, adapter.FormatAnthropic)

	sh.setUsageTokens(10, -1)
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "tokens-used", "inputTokens": float64(12), "outputTokens": float64(34)}})

	// finishing should keep upstream usage (useUpstreamUsage=true)
	sh.finishResponse("end_turn")
	testutil.Falsef(t, sh.inputTokens != 12 || sh.outputTokens != 34, "unexpected usage: in=%d out=%d", sh.inputTokens, sh.outputTokens)
}

func TestStreamHandler_UsageMissingFieldsKeepLocalEstimate(t *testing.T) {
	sh := newStreamHandler(&config.Config{}, newFlushRecorder(), debug.New(false, false), false, adapter.FormatAnthropic)
	defer sh.release()
	sh.inputTokens, sh.outputTokens = 31, 17
	sh.applyUpstreamUsageTokens(map[string]any{"output_tokens": 9})
	testutil.Falsef(t, !sh.useUpstreamUsage || sh.inputTokens != 31 || sh.outputTokens != 9, "partial usage overwrote estimate: input=%d output=%d", sh.inputTokens, sh.outputTokens)
	sh.resetRoundState()
	sh.inputTokens, sh.outputTokens = 31, 17
	sh.applyUpstreamUsageTokens(map[string]any{"unrelated": 0.25})
	testutil.Falsef(t, sh.useUpstreamUsage || sh.inputTokens != 31 || sh.outputTokens != 17, "usage without token fields replaced the estimate: input=%d output=%d", sh.inputTokens, sh.outputTokens)
}

func TestStreamHandler_DetailedUsageIsAssignedIdempotently(t *testing.T) {
	sh := newStreamHandler(&config.Config{}, newFlushRecorder(), debug.New(false, false), false, adapter.FormatAnthropic)
	defer sh.release()
	usage := map[string]any{
		"inputTokens": 1000, "outputTokens": 20, "cacheReadTokens": 900,
		"cacheWriteTokens": 50, "reasoningTokens": 7,
	}
	sh.handleMessage(upstream.SSEMessage{Type: "model.tokens-used", Event: usage})
	sh.handleMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]any{"usage": usage}})
	testutil.Falsef(t, sh.inputTokens != 1000 || sh.outputTokens != 20 || sh.cachedInputTokens != 900 || sh.cacheWriteTokens != 50 || sh.reasoningTokens != 7, "detailed usage lost or doubled: in=%d out=%d cached=%d write=%d reasoning=%d", sh.inputTokens, sh.outputTokens, sh.cachedInputTokens, sh.cacheWriteTokens, sh.reasoningTokens)
	sh.resetRoundState()
	testutil.Falsef(t, sh.cachedInputTokens != 0 || sh.cacheWriteTokens != 0 || sh.reasoningTokens != 0, "detailed usage survived round reset: %+v", sh)
}

func TestStreamHandler_FinalOutputTokens_MatchChunkedText(t *testing.T) {
	sh, _ := newStreamTestHandler(t, false, adapter.FormatAnthropic)

	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "text-start"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "hel"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "lo world!"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "text-end"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}})

	want := tiktoken.EstimateTextTokens("hello world!")
	testutil.Equal(t, sh.outputTokens, want)
}

func TestStreamHandler_KeepAlive_NoPanic(t *testing.T) {
	sh, rec := newStreamTestHandler(t, true, adapter.FormatAnthropic)

	// should not write once terminal state is set
	sh.mu.Lock()
	sh.hasReturn = true
	sh.returned.Store(true)
	sh.mu.Unlock()
	sh.writeKeepAlive()
	testutil.Equal(t, rec.buf.Len(), 0)

	// reset: a silent stream must stay uncommitted until it has content.
	sh.mu.Lock()
	sh.hasReturn = false
	sh.returned.Store(false)
	sh.mu.Unlock()
	sh.writeKeepAlive()
	testutil.Equal(t, rec.buf.Len(), 0)
	sh.handleMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": "hello"}})
	sh.writeKeepAlive()
	// An Anthropic stream is kept alive with the ping event, which clients
	// count as stream activity; a comment is invisible to their watchdogs.
	testutil.MustContain(t, rec.buf.String(), ssePing)
}

func TestResponseMessageID_OpenAIUsesChatCompletionPrefix(t *testing.T) {
	id := responseMessageID(adapter.FormatOpenAI)
	testutil.Falsef(t, !strings.HasPrefix(id, "chatcmpl-"), "id=%q want chatcmpl- prefix", id)
	testutil.NotEqual(t, id, responseMessageID(adapter.FormatOpenAI))
}

func TestResponseMessageID_AnthropicKeepsMessagePrefix(t *testing.T) {
	id := responseMessageID(adapter.FormatAnthropic)
	testutil.Falsef(t, !strings.HasPrefix(id, "msg_"), "id=%q want msg_ prefix", id)
}

func TestStreamHandler_ReasoningCountsAsOutputAndReachesClient(t *testing.T) {
	sh, rec := newStreamTestHandler(t, true, adapter.FormatAnthropic)

	sh.handleMessage(upstream.SSEMessage{
		Type:  "model.reasoning-delta",
		Event: map[string]any{"delta": "already generated and potentially billed"},
	})

	testutil.True(t, sh.hasAnyOutput(), "reasoning must count as upstream output to prevent a billed retry")
	testutil.MustContain(t, rec.buf.String(), "already generated and potentially billed")
}
