package handler

import (
	"net/http"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/middleware"
)

// HandleCountTokens handles /v1/messages/count_tokens requests.
func (h *Handler) HandleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ClaudeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if !middleware.APIKeyAllowsModel(r.Context(), req.Model) {
		apperrors.New("permission_error", "API key is not allowed to use model "+strings.TrimSpace(req.Model), http.StatusForbidden).WriteResponse(w)
		return
	}

	// A handler without a config still has to answer a token count: the debug
	// logger is optional, and this endpoint is on the critical path of every
	// client that budgets its context before sending a completion.
	cfg := h.configSnapshot()
	if cfg == nil {
		cfg = &config.Config{}
	}
	logger := debug.NewForContext(r.Context(), cfg.DebugEnabled, cfg.DebugLogSSE)
	defer logger.Close()
	logger.LogIncomingRequest(req)

	// The channel is what picks the token profile, and on the unified prefix the
	// path names no channel at all — only the model does. A path-only lookup here
	// silently returned the generic estimate for every /v1 request, so a client
	// that budgets its context against count_tokens planned against the wrong
	// number. Channel names are compared case-insensitively everywhere else; the
	// stored value is whatever the operator's catalog spells, so normalize it.
	channel := strings.ToLower(h.ModelChannel(r, req.Model))
	breakdown := estimateRequestTokenBreakdown(req)
	profile := channel

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{
		"input_tokens":   breakdown.Total,
		"prompt_profile": profile,
		"breakdown": map[string]int{
			"base_prompt_tokens":    breakdown.BasePromptTokens,
			"system_context_tokens": breakdown.SystemContextTokens,
			"history_tokens":        breakdown.HistoryTokens,
			"tools_tokens":          breakdown.ToolsTokens,
		},
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		_ = err
	}
}
