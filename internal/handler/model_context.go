package handler

import (
	"context"
	"strings"

	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

// Model context windows.
//
// A client that cannot see a model's window falls back to its own default
// (pi-ai uses 262144, Codex uses its compiled-in table). The fallback is wrong in
// both directions: too small and a legitimate long session is reported as a
// context overflow after the model already answered, too large and the client
// keeps sending a history the upstream will refuse. The gateway already knows
// the real number — it forwards it upstream — so the public model list has to
// report the same observation.
//
// Nothing here is invented. A model whose window was never observed reports
// nothing at all, which is honest: the client then applies its own default rather
// than a number this process made up.

// modelContextWindows is the observed window per channel and model id.
type modelContextWindows struct {
	// input is keyed by contextKey(channel, model id).
	input map[string]int
	// output carries the declared output budget where the catalog published one.
	output map[string]int
}

func contextKey(channel, modelID string) string {
	return strings.ToLower(strings.TrimSpace(channel)) + "\x00" + strings.ToLower(strings.TrimSpace(modelID))
}

// lookup answers the input window and output budget for one model. A zero means
// "never observed" and must be rendered as an absent field, not as a zero.
func (w *modelContextWindows) lookup(channel string, modelIDs ...string) (int, int) {
	if w == nil {
		return 0, 0
	}
	for _, modelID := range modelIDs {
		if strings.TrimSpace(modelID) == "" {
			continue
		}
		key := contextKey(channel, modelID)
		input, hasInput := w.input[key]
		output, hasOutput := w.output[key]
		if hasInput || hasOutput {
			return input, output
		}
	}
	return 0, 0
}

// observedModelContextWindows reads every window this deployment has already
// observed from the upstream catalogs. The accounts are read once and shared, so
// an account-scoped window table and an account-global one are filled from the
// same pass.
func (h *Handler) observedModelContextWindows(ctx context.Context) *modelContextWindows {
	w := &modelContextWindows{input: map[string]int{}, output: map[string]int{}}
	accounts, err := h.enabledAccountsForContextWindows(ctx)
	if err != nil {
		return w
	}

	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") {
			for _, profile := range acc.GrokModelCatalog {
				key := contextKey("grok", profile.ModelID)
				// A public route may select any eligible account, so publish the
				// smallest observed positive budget rather than over-promising.
				if profile.ContextWindow > 0 {
					if old := w.input[key]; old == 0 || profile.ContextWindow < old {
						w.input[key] = profile.ContextWindow
					}
				}
				if profile.MaxCompletionTokens > 0 {
					if old := w.output[key]; old == 0 || profile.MaxCompletionTokens < old {
						w.output[key] = profile.MaxCompletionTokens
					}
				}
			}
		}
		// The channel declares its own windows: Qoder's catalog carries
		// max_input_tokens per model, WorkBuddy carries input and output. Cline
		// names models but not windows, so it registers none and contributes
		// nothing here.
		channel := strings.ToLower(strings.TrimSpace(acc.AccountType))
		input, output := provider.ContextWindows(channel, acc)
		if len(input) > 0 {
			mergeInputWindows(w, channel, input)
		}
		for modelID, limit := range output {
			key := contextKey(channel, modelID)
			if existing, ok := w.output[key]; !ok || limit > existing {
				w.output[key] = limit
			}
		}
	}

	// Missing catalog metadata remains unknown.
	return w
}

func (h *Handler) enabledAccountsForContextWindows(ctx context.Context) ([]*store.Account, error) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return nil, nil
	}
	return h.loadBalancer.Store.GetEnabledAccounts(ctx)
}

func mergeInputWindows(w *modelContextWindows, channel string, windows map[string]int) {
	for modelID, limit := range windows {
		if limit <= 0 {
			continue
		}
		key := contextKey(channel, modelID)
		if existing, ok := w.input[key]; !ok || limit > existing {
			w.input[key] = limit
		}
	}
}

// modelContextWindow omits budgets which were not observed upstream.
func (h *Handler) modelContextWindow(windows *modelContextWindows, channel, modelID, publicID string) (int, int) {
	return windows.lookup(channel, modelID, publicID)
}
