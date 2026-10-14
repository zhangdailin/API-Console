package grok

import (
	"context"
	"fmt"
	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
	"strings"
	"time"
)

func (h *Handler) isModelValidationCached(modelID string) bool {
	if h == nil || strings.TrimSpace(modelID) == "" {
		return false
	}
	h.modelCacheMu.RLock()
	expiresAt, ok := h.modelCache[modelID]
	h.modelCacheMu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().Before(expiresAt) {
		return true
	}
	h.modelCacheMu.Lock()
	if staleAt, ok := h.modelCache[modelID]; ok && !time.Now().Before(staleAt) {
		delete(h.modelCache, modelID)
	}
	h.modelCacheMu.Unlock()
	return false
}

func (h *Handler) cacheValidatedModel(modelID string) {
	if h == nil || strings.TrimSpace(modelID) == "" {
		return
	}
	h.modelCacheMu.Lock()
	if h.modelCache == nil {
		h.modelCache = make(map[string]time.Time)
	}
	h.modelCache[modelID] = time.Now().Add(grokModelValidationCacheTTL)
	h.modelCacheMu.Unlock()
}

func (h *Handler) ensureModelEnabled(ctx context.Context, modelID string) error {
	id := normalizeModelID(modelID)
	if h.isModelValidationCached(id) {
		return nil
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		// Without a store there is no observed catalog to validate against. A
		// compiled-in model list is not a substitute: it would accept a model no
		// active account ever advertised.
		return fmt.Errorf("model not found")
	}

	m, err := h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", id)
	if err != nil || m == nil || !modelpolicy.IsVisibleGrokModel(id, m.Verified) {
		return fmt.Errorf("model not found")
	}
	if !m.Status.Enabled() {
		return fmt.Errorf("model not available")
	}
	channel := strings.TrimSpace(m.Channel)
	channel = util.FirstNonEmptyUntrimmed(channel, "grok")
	if !strings.EqualFold(channel, "grok") {
		return fmt.Errorf("model not found")
	}
	h.cacheValidatedModel(id)
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
	if h == nil || h.lb == nil || h.lb.Store == nil || strings.TrimSpace(spec.ID) == "" {
		return spec
	}
	model, err := h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", normalizeModelID(spec.ID))
	if err != nil || model == nil {
		return spec
	}
	upstream := strings.TrimSpace(model.UpstreamModel)
	upstream = util.FirstNonEmptyUntrimmed(upstream, spec.UpstreamModel)
	// Build route rows refine the upstream model name.
	if strings.EqualFold(strings.TrimSpace(model.Provider), ProviderBuild) && upstream != "" {
		spec.UpstreamModel = upstream
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

func modelNotFoundMessage(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return "The model does not exist or you do not have access to it."
	}
	return fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", modelID)
}

func modelValidationMessage(modelID string, err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if strings.EqualFold(msg, "model not found") {
		return modelNotFoundMessage(modelID)
	}
	return msg
}
