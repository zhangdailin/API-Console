package grok

import (
	"context"
	"fmt"
	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/store"
	"strings"
)

func (h *Handler) ensureModelEnabled(ctx context.Context, modelID string) error {
	id := normalizeModelID(modelID)
	if h != nil && h.modelCache.Has(id) {
		return nil
	}
	if h == nil {
		return fmt.Errorf("model not found")
	}
	model, err := grokbase.EnabledModel(ctx, h.affinityStore(), id)
	if err != nil {
		return err
	}
	if channel := strings.TrimSpace(model.Channel); channel != "" && !strings.EqualFold(channel, "grok") {
		return fmt.Errorf("model not found")
	}
	h.modelCache.Remember(id)
	return nil
}

func (h *Handler) ensureResolvedModelCapability(ctx context.Context, modelID string, spec ModelSpec, capability string) error {
	if err := h.ensureResolvedModelEnabled(ctx, modelID, spec); err != nil {
		return err
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return nil
	}
	model, err := h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", normalizeModelID(modelID))
	if err == nil && model != nil && !model.SupportsCapability(capability) {
		return fmt.Errorf("model %s does not support %s", normalizeModelID(modelID), strings.ToLower(strings.TrimSpace(capability)))
	}
	return nil
}

// resolveConversationModel routes only the enabled accounts' observed catalog.
func (h *Handler) resolveConversationModel(ctx context.Context, modelID string) (ModelSpec, bool) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return ModelSpec{}, false
	}
	accounts, err := h.lb.Store.GetEnabledAccounts(ctx)
	if err != nil {
		return ModelSpec{}, false
	}
	catalogs := make([][]modelcatalog.Profile, 0, len(accounts))
	for _, acc := range accounts {
		if !accountpolicy.IsGrokAccount(acc) || !store.AccountAuthActive(acc) || strings.TrimSpace(acc.StatusCode) == "401" {
			continue
		}
		catalogs = append(catalogs, modelpolicy.GrokAccountCatalog(acc))
	}
	spec, effort, ok := ResolveModelAlias(modelID, catalogs...)
	if !ok {
		return ModelSpec{}, false
	}
	spec.AliasReasoningEffort = effort
	spec = h.applyPersistedRoute(ctx, spec)
	for _, acc := range accounts {
		if accountpolicy.IsGrokAccount(acc) && store.AccountAuthActive(acc) && strings.TrimSpace(acc.StatusCode) != "401" && AccountSupportsModel(acc, spec.UpstreamModel) && h.routeAllowsAccount(ctx, spec.ID, acc.ID) {
			return spec, true
		}
	}
	return ModelSpec{}, false
}

// applyPersistedRoute overlays the control-plane route on an observed model
// shape. Capabilities remain catalog-owned, while the upstream model name is
// operator-controlled and survives restarts in the model store.
func (h *Handler) applyPersistedRoute(ctx context.Context, spec ModelSpec) ModelSpec {
	if h != nil {
		spec.UpstreamModel = grokbase.UpstreamRoute(ctx, h.affinityStore(), normalizeModelID(spec.ID), ProviderBuild, spec.UpstreamModel)
	}
	return spec
}

func (h *Handler) ensureResolvedModelEnabled(ctx context.Context, modelID string, spec ModelSpec) error {
	err := h.ensureModelEnabled(ctx, modelID)
	if err == nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(err.Error()), "model not found") {
		return err
	}
	// Dynamic Build capabilities are authoritative even before the admin model
	// table has been reconciled by the next catalog refresh.
	if dynamic, ok := h.resolveConversationModel(ctx, modelID); ok && strings.EqualFold(dynamic.UpstreamModel, spec.UpstreamModel) {
		return nil
	}
	return err
}
