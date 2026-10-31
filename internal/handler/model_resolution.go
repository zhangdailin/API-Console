package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"orchids-api/internal/store"
	"strings"
)

func normalizeRequestedModelID(modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	return modelID
}

// lookupModelRow resolves a requested model id to its catalog row. The id is
// matched in normalized form; a non-empty channel limits the lookup to that
// channel's catalog. It returns nil when no row matches, the id is empty, or the
// store is unavailable.
func (h *Handler) lookupModelRow(ctx context.Context, channel, modelID string) *store.Model {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return nil
	}
	candidate := normalizeRequestedModelID(modelID)
	if candidate == "" {
		return nil
	}
	if channel == "" {
		return nil
	}
	m, err := h.loadBalancer.Store.GetModelByChannelAndModelID(ctx, channel, candidate)
	if err != nil || m == nil {
		return nil
	}
	return m
}

// requestReasoningEffort returns the effort a client asked for, from whichever
// dialect it used: OpenAI's reasoning_effort or Anthropic's
// output_config.effort / thinking. A thinking budget without an explicit effort
// maps onto the same coarse levels so an effort-suffixed catalog can still be
// resolved.
//
// An explicit `thinking: {"type":"disabled"}` is a stated choice, not an
// absence: it yields "none", which every channel reads as "no reasoning",
// instead of letting the channel default (high) stand in. It wins over
// output_config.effort, as on the native Grok path.
func requestReasoningEffort(req ClaudeRequest) string {
	if effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort)); effort != "" {
		return effort
	}
	if typeName, _ := req.Thinking["type"].(string); strings.EqualFold(strings.TrimSpace(typeName), "disabled") {
		return "none"
	}
	for _, config := range []map[string]any{req.OutputConfig, req.Thinking} {
		value, ok := config["effort"].(string)
		if !ok {
			continue
		}
		if effort := strings.ToLower(strings.TrimSpace(value)); effort != "" {
			return effort
		}
	}
	if req.Thinking == nil {
		return ""
	}
	budget, ok := looseNumber(req.Thinking["budget_tokens"])
	if !ok || budget <= 0 {
		return ""
	}
	switch {
	case budget < 4096:
		return "low"
	case budget < 16384:
		return "medium"
	default:
		return "high"
	}
}

// looseNumber accepts the numeric shapes JSON decoding produces.
func looseNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

// effortVariantOrder is the fallback order tried when a client asks for a model
// family by its bare name and the catalog only exposes effort-suffixed
// variants. A catalog may publish models as "<family>-<effort>" (gpt-5-6-sol-low),
// while clients such as Codex send the family name plus reasoning_effort.
var effortVariantOrder = []string{"medium", "high", "low", "xhigh", "max"}

// resolveEffortModelVariant maps a bare model name onto the catalog entry that
// actually exists. An exact catalog hit always wins; otherwise the requested
// reasoning_effort is tried as a suffix and then the default effort order.
// Without this, "gpt-5-6-sol" is rejected as "model not found" even though the
// family is available and the client stated which effort it wants.
//
// A model that already carries an effort suffix is never suffixed again: the
// old code would have tried "gpt-5-6-sol-low-low" and then fallen through to
// "-medium", silently serving an effort the client never asked for.
func (h *Handler) resolveEffortModelVariant(ctx context.Context, modelID, effort, forcedChannel string) string {
	modelID = normalizeRequestedModelID(modelID)
	if modelID == "" || h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return modelID
	}
	lookup := func(id string) *store.Model { return h.lookupModelRow(ctx, forcedChannel, id) }
	if m := lookup(modelID); m != nil {
		return modelID
	}
	// "gpt-5-6-sol-low" is final: it is an exact catalog entry under a name that
	// happens to end in an effort word, and appending another suffix can only
	// produce a wrong model.
	if _, level := splitEffortVariantSuffix(modelID); level != "" {
		return modelID
	}

	effort = strings.ToLower(strings.TrimSpace(effort))
	candidates := make([]string, 0, len(effortVariantOrder)+1)
	if effort != "" {
		candidates = append(candidates, modelID+"-"+effort)
	}
	for _, suffix := range effortVariantOrder {
		candidates = append(candidates, modelID+"-"+suffix)
	}
	for _, candidate := range candidates {
		if m := lookup(candidate); m != nil && m.Status.Enabled() {
			slog.Info("Resolved bare model to an effort variant",
				"model", modelID, "resolved", candidate, "reasoning_effort", effort, "channel", forcedChannel)
			return candidate
		}
	}
	return modelID
}

func (h *Handler) validateModelAvailability(ctx context.Context, modelID, forcedChannel string) (*store.Model, error) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return nil, nil
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, nil
	}
	m := h.lookupModelRow(ctx, forcedChannel, modelID)
	if m == nil {
		return nil, fmt.Errorf("model not found")
	}
	if !m.Status.Enabled() {
		return nil, fmt.Errorf("model not available")
	}
	// A channel-pinned path must not serve a model that belongs to another
	// channel: it is reported as simply unknown for that route.
	if forcedChannel != "" && !sameModelChannel(m.Channel, forcedChannel) {
		return nil, fmt.Errorf("model not found")
	}
	return m, nil
}

func sameModelChannel(a, b string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.ReplaceAll(value, "_", "-")
		return strings.ReplaceAll(value, " ", "-")
	}
	return normalize(a) == normalize(b)
}
