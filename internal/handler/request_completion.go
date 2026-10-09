package handler

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/adapter"
	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/middleware"
	"orchids-api/internal/pricing"
	"orchids-api/internal/store"
)

func (h *Handler) finishMessages(w http.ResponseWriter, r *http.Request, sh *streamHandler, req ClaudeRequest, currentAccount *store.Account, forcedChannel string, isStream bool, responseFormat adapter.ResponseFormat, startTime time.Time, logger *debug.Logger) {
	// Ensure a final response
	if !sh.hasReturn {
		sh.finishResponse("end_turn")
	}
	if !isStream && !sh.requestFailed {
		stopReason := sh.finalStopReason
		if stopReason == "" {
			stopReason = "end_turn"
		}

		for i := range sh.contentBlocks {
			blockType, _ := sh.contentBlocks[i]["type"].(string)
			switch blockType {
			case "text":
				materializeBlockField(sh.contentBlocks[i], "text", sh.textBlockBuilders, i)
			case "thinking":
				materializeBlockField(sh.contentBlocks[i], "thinking", sh.thinkingBlockBuilders, i)
			}
		}

		if len(sh.contentBlocks) == 0 && sh.responseText.Len() > 0 {
			sh.contentBlocks = append(sh.contentBlocks, map[string]interface{}{
				"type": "text",
				"text": sh.responseText.String(),
			})
		}
		if sh.contentBlocks == nil {
			sh.contentBlocks = make([]map[string]interface{}, 0)
		}

		var response interface{}
		if responseFormat == adapter.FormatOpenAI {
			response = buildOpenAINonStreamResponse(sh, req.Model, stopReason)
		} else {
			anthropicResponse := map[string]interface{}{
				"id":            sh.msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       sh.contentBlocks,
				"model":         req.Model,
				"stop_reason":   stopReason,
				"stop_sequence": nil,
				"usage": map[string]int{
					"input_tokens":  sh.inputTokens,
					"output_tokens": sh.outputTokens,
				},
			}
			response = anthropicResponse
		}

		if err := json.NewEncoder(w).Encode(response); err != nil {
			sh.markWriteError("nonstream_response", err)
			slog.Error("Failed to write JSON response", "error", err)
		}

	}

	// Sync state and update stats using helpers. A failed request with no
	// provider-reported usage must not turn the local input estimate into spend;
	// still count the request itself for operational history.
	statsInput, statsOutput := sh.inputTokens, sh.outputTokens
	if sh.requestFailed && !sh.useUpstreamUsage {
		statsInput, statsOutput = 0, 0
	}
	h.updateAccountStats(r.Context(), currentAccount, statsInput, statsOutput)

	// Audit log
	if h.auditLogger != nil {
		accountID := int64(0)
		channel := forcedChannel
		if currentAccount != nil {
			accountID = currentAccount.ID
		}
		status := "success"
		if sh.requestFailed || (sh.finalStopReason == "" && !sh.hasReturn) {
			status = "error"
		}
		usageSource := audit.UsageSourceEstimated
		if sh.useUpstreamUsage {
			usageSource = audit.UsageSourceUpstream
		}
		metadata := map[string]interface{}{
			"stream":               isStream,
			"finish_reason":        sh.finalStopReason,
			"requested_max_tokens": req.outputTokenLimit(),
		}
		visibleOutput, reasoningOutput, toolOutput := false, false, false
		for _, block := range sh.contentBlocks {
			switch block["type"] {
			case "text":
				text, _ := block["text"].(string)
				visibleOutput = visibleOutput || text != ""
			case "thinking":
				text, _ := block["thinking"].(string)
				reasoningOutput = reasoningOutput || text != ""
			case "tool_use":
				toolOutput = true
			}
		}
		metadata["visible_output"] = visibleOutput
		metadata["reasoning_only"] = reasoningOutput && !visibleOutput && !toolOutput
		for key, value := range sh.usageMetadata {
			metadata[key] = value
		}
		event := audit.Event{
			// One journal schema for every channel: the log centre must be able to
			// compare two channels' requests on the same fields.
			Kind:              audit.KindRequest,
			RequestID:         middleware.GetRequestID(r.Context()),
			Action:            "chat_request",
			APIKeyID:          middleware.APIKeyID(r.Context()),
			AccountID:         accountID,
			Model:             req.Model,
			Channel:           channel,
			ClientIP:          r.RemoteAddr,
			UserAgent:         r.UserAgent(),
			Duration:          time.Since(startTime).Milliseconds(),
			Status:            status,
			Metadata:          metadata,
			InputTokens:       sh.inputTokens,
			CachedInputTokens: sh.cachedInputTokens,
			CacheWriteTokens:  sh.cacheWriteTokens,
			ReasoningTokens:   sh.reasoningTokens,
			OutputTokens:      sh.outputTokens,
			TotalTokens:       sh.inputTokens + sh.outputTokens,
			UsageSource:       usageSource,
		}
		// Settle the reservation taken before the request and price the same
		// event, so the journal answers "what did this cost" and the key's
		// balance moves exactly once. An estimated row is never charged.
		if result, priced := middleware.SettleAPIKeyBilling(
			r.Context(), nil, req.Model, usageSource, int64(sh.inputTokens), int64(sh.cachedInputTokens), int64(sh.outputTokens),
		); priced {
			event.CostInUSDTicks = result.CostInUSDTicks
			event.PricingModel = result.Model
			event.PricingVersion = pricing.Version
		}
		h.auditLogger.Log(r.Context(), event)
	}
}

func toolChoiceDisablesTools(choice interface{}) bool {
	switch typed := choice.(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "none")
	case map[string]interface{}:
		return strings.EqualFold(strings.TrimSpace(fmt.Sprint(typed["type"])), "none")
	default:
		return false
	}
}

func randomSessionID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// Fallback to time-based if crypto/rand fails (unlikely)
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
