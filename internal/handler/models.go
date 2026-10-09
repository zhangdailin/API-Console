package handler

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/channel"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/middleware"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/store"
)

type PublicModelResponse struct {
	ID            string   `json:"id"`
	Object        string   `json:"object"`
	Created       int64    `json:"created"`
	OwnedBy       string   `json:"owned_by"`
	Capabilities  []string `json:"capabilities,omitempty"`
	Provider      string   `json:"provider,omitempty"`
	UpstreamModel string   `json:"upstream_model,omitempty"`
	BillingTier   string   `json:"billing_tier,omitempty"`
	BillingSource string   `json:"billing_source,omitempty"`
	// ContextLength is the model's real input-token window, as observed from the
	// channel's own catalog. It is omitted when nothing was observed, because a
	// client that reads a wrong number budgets against the wrong number: too low
	// and it reports a context overflow after the model already answered, too high
	// and it keeps sending a history the upstream will refuse.
	ContextLength int `json:"context_length,omitempty"`
	// MaxInputTokens carries the same observation under the name some clients
	// look for. Both are emitted so a client that only reads one still finds it.
	MaxInputTokens int `json:"max_input_tokens,omitempty"`
	// MaxOutputTokens is the declared output budget where the catalog publishes
	// one. Zero means unobserved and is omitted.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// Grok Build publishes these fields per account. Pointer booleans preserve
	// the important distinction between an explicit false and unknown metadata.
	ReasoningEfforts        []string `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort  string   `json:"default_reasoning_effort,omitempty"`
	SupportsReasoningEffort *bool    `json:"supports_reasoning_effort,omitempty"`
	SupportsBackendSearch   *bool    `json:"supports_backend_search,omitempty"`
}

type PublicModelsListResponse struct {
	Object string                `json:"object"`
	Data   []PublicModelResponse `json:"data"`
}

// legacyModelCreated is the placeholder the API used before a route row carried
// its own creation time. It stays only for rows stored by an older build.
const legacyModelCreated = 1677610602

func publicModelResponse(id, ownedBy string, createdAt time.Time) PublicModelResponse {
	created := int64(legacyModelCreated)
	if !createdAt.IsZero() {
		created = createdAt.Unix()
	}
	return PublicModelResponse{ID: id, Object: "model", Created: created, OwnedBy: ownedBy}
}

func isVisiblePublicModel(m *store.Model, filterChannel string) (string, bool) {
	if m == nil {
		return "", false
	}

	mChannel := strings.TrimSpace(m.Channel)
	if filterChannel != "" && !strings.EqualFold(mChannel, filterChannel) || !m.Status.Enabled() {
		return mChannel, false
	}
	if strings.EqualFold(mChannel, "grok") && !modelpolicy.IsVisibleGrokModel(m.ModelID, m.Verified) {
		return mChannel, false
	}
	return mChannel, true
}

// externalPublicModelID is the name clients see for a route row. Grok routes
// may carry a Build qualifier internally; clients receive the bare model name.
func externalPublicModelID(channel, internalID string) string {
	if strings.EqualFold(strings.TrimSpace(channel), "grok") {
		if external := modelpolicy.ExternalPublicID(internalID); external != "" {
			return external
		}
	}
	return normalizeRequestedModelID(internalID)
}

func publicModelIDKey(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

// grokBuildProfiles returns the conservative capability view shared by every
// enabled Build account that advertised a model.
func (h *Handler) grokBuildProfiles(ctx context.Context) map[string]modelcatalog.Profile {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return nil
	}
	accounts, err := h.loadBalancer.Store.GetEnabledAccounts(ctx)
	if err != nil {
		return nil
	}
	catalogs := make([][]modelcatalog.Profile, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(acc.GrokProvider))
		if provider == "" && strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") {
			provider = "build"
		}
		if provider != "build" || len(acc.GrokModelCatalog) == 0 {
			continue
		}
		catalogs = append(catalogs, acc.GrokModelCatalog)
	}
	if len(catalogs) == 0 {
		return nil
	}
	profiles := modelcatalog.Aggregate(catalogs...)
	out := make(map[string]modelcatalog.Profile, len(profiles))
	for _, profile := range profiles {
		for _, id := range []string{profile.ModelID, modelpolicy.ExternalPublicID(profile.ModelID)} {
			if key := publicModelIDKey(id); key != "" {
				out[key] = profile
			}
		}
	}
	return out
}

func applyGrokBuildProfile(entry *PublicModelResponse, profile modelcatalog.Profile) {
	if entry == nil {
		return
	}
	entry.ReasoningEfforts = append([]string(nil), profile.ReasoningEfforts...)
	entry.DefaultReasoningEffort = profile.DefaultReasoningEffort
	supportsReasoning := profile.SupportsReasoningEffort
	supportsSearch := profile.SupportsBackendSearch
	entry.SupportsReasoningEffort = &supportsReasoning
	entry.SupportsBackendSearch = &supportsSearch
	if profile.ContextWindow > 0 {
		entry.ContextLength = profile.ContextWindow
		entry.MaxInputTokens = profile.ContextWindow
	}
	if profile.MaxCompletionTokens > 0 {
		entry.MaxOutputTokens = profile.MaxCompletionTokens
	}
}

func appendGrokReasoningVariants(items []PublicModelResponse, seen map[string]struct{}, entry PublicModelResponse) []PublicModelResponse {
	if !strings.EqualFold(entry.OwnedBy, "grok") {
		return items
	}
	base := modelpolicy.GrokModelSlug(entry.ID)
	aliases := make([]string, 0, 6)
	// Publish only effort aliases supported by the Build model contract.
	levels := modelpolicy.SupportedReasoningEfforts(entry.ID)
	if len(levels) >= 2 {
		for _, level := range levels {
			aliases = append(aliases, base+"-"+level)
		}
	}
	for _, alias := range aliases {
		key := publicModelIDKey(alias)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		copy := entry
		copy.ID = alias
		items = append(items, copy)
	}
	return items
}

func (h *Handler) HandleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apperrors.New("invalid_request_error", "Method not allowed", http.StatusMethodNotAllowed).WriteResponse(w)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// Determine channel filter based on path prefix
	filterChannel := channelFromPath(r.URL.Path)

	ctx := r.Context()
	if h.loadBalancer == nil || h.loadBalancer.Store == nil {
		apperrors.New("api_error", "Model store not configured", http.StatusServiceUnavailable).WriteResponse(w)
		return
	}
	allModels, err := h.loadBalancer.Store.ListModels(ctx)
	if err != nil {
		apperrors.New("api_error", "Failed to fetch models: "+err.Error(), http.StatusInternalServerError).WriteResponse(w)
		return
	}
	sort.SliceStable(allModels, func(i, j int) bool {
		if allModels[i] == nil || allModels[j] == nil {
			return allModels[j] == nil
		}
		if allModels[i].SortOrder != allModels[j].SortOrder {
			return allModels[i].SortOrder < allModels[j].SortOrder
		}
		return strings.ToLower(allModels[i].ModelID) < strings.ToLower(allModels[j].ModelID)
	})
	var publicModels []PublicModelResponse
	seenPublicModelIDs := make(map[string]struct{}, len(allModels))
	// One read of the observed catalogs answers every row below, so the model
	// list reports the same window the request path forwards upstream.
	contextWindows := h.observedModelContextWindows(ctx)
	grokProfiles := h.grokBuildProfiles(ctx)
	for _, m := range allModels {
		mChannel, ok := isVisiblePublicModel(m, filterChannel)
		if !ok {
			continue
		}
		// A Build qualifier is an internal routing detail; publish the bare name.
		publicID := externalPublicModelID(mChannel, m.ModelID)
		if !middleware.APIKeyAllowsModel(ctx, m.ModelID) && !middleware.APIKeyAllowsModel(ctx, publicID) {
			continue
		}
		// Publish one public row per external Build model ID.
		publicIDKey := publicModelIDKey(publicID)
		if _, duplicate := seenPublicModelIDs[publicIDKey]; duplicate {
			continue
		}
		seenPublicModelIDs[publicIDKey] = struct{}{}

		entry := publicModelResponse(publicID, mChannel, m.CreatedAt)
		entry.Capabilities = m.Capabilities
		entry.Provider = m.Provider
		entry.UpstreamModel = m.UpstreamModel
		entry.BillingTier = m.BillingTier
		entry.BillingSource = m.BillingSource
		// The window is looked up by the route's own id first: that is what the
		// channel's catalog was keyed by when it was observed. The public alias is
		// the fallback for a channel that publishes a different spelling.
		input, output := h.modelContextWindow(ctx, contextWindows, mChannel, m.ModelID, publicID)
		entry.ContextLength = input
		entry.MaxInputTokens = input
		entry.MaxOutputTokens = output
		if strings.EqualFold(mChannel, "grok") {
			provider := strings.ToLower(strings.TrimSpace(m.Provider))
			if provider == "build" || provider == "" {
				for _, id := range []string{m.ModelID, m.UpstreamModel, publicID} {
					if profile, ok := grokProfiles[publicModelIDKey(id)]; ok {
						applyGrokBuildProfile(&entry, profile)
						break
					}
				}
			}
		}
		publicModels = append(publicModels, entry)
		publicModels = appendGrokReasoningVariants(publicModels, seenPublicModelIDs, entry)
	}

	// Codex-family clients ask for a richer catalog that carries the context
	// window, input modalities and reasoning levels the OpenAI list omits.
	if strings.TrimSpace(r.URL.Query().Get("client_version")) != "" {
		writeCodexModelCatalog(w, r, newCodexModelCatalog(publicModels))
		return
	}

	resp := PublicModelsListResponse{
		Object: "list",
		Data:   publicModels,
	}

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		apperrors.New("api_error", "Failed to encode response", http.StatusInternalServerError).WriteResponse(w)
	}
}

// HandleModelByID is optional for public API but good for completeness
func (h *Handler) HandleModelByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apperrors.New("invalid_request_error", "Method not allowed", http.StatusMethodNotAllowed).WriteResponse(w)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	path := r.URL.Path
	providerID, id, matched := channel.TrimModelPath(path)
	if !matched {
		apperrors.New("invalid_request_error", "Model route not found", http.StatusNotFound).WriteResponse(w)
		return
	}
	filterChannel := string(providerID)

	if id == "" {
		apperrors.New("invalid_request_error", "Model ID required", http.StatusBadRequest).WriteResponse(w)
		return
	}
	if !middleware.APIKeyAllowsModel(r.Context(), id) {
		apperrors.New("invalid_request_error", "Model not found", http.StatusNotFound).WriteResponse(w)
		return
	}

	ctx := r.Context()
	if h.loadBalancer == nil || h.loadBalancer.Store == nil {
		apperrors.New("api_error", "Model store not configured", http.StatusServiceUnavailable).WriteResponse(w)
		return
	}

	m, err := h.loadBalancer.Store.GetModelByChannelAndModelID(ctx, filterChannel, id)
	if err != nil || m == nil {
		// The catalog collapses "<family>-<effort>" variants into one entry and
		// advertises the *family* slug, so a client validating a catalog entry
		// asks for a name no row carries. Resolve it onto the variant the request
		// path would use; answering 404 here is what pushes a client back to
		// guessing suffixes.
		if variant := h.resolveEffortModelVariant(ctx, id, "", filterChannel); variant != "" && variant != id {
			m, err = h.loadBalancer.Store.GetModelByChannelAndModelID(ctx, filterChannel, variant)
		}
	}
	if err != nil || m == nil {
		apperrors.New("invalid_request_error", "Model not found", http.StatusNotFound).WriteResponse(w)
		return
	}
	mChannel, ok := isVisiblePublicModel(m, filterChannel)
	if !ok {
		apperrors.New("invalid_request_error", "Model not found", http.StatusNotFound).WriteResponse(w)
		return
	}

	resp := publicModelResponse(m.ModelID, mChannel, m.CreatedAt)
	// The single-model lookup answers with the same window the list publishes, so
	// a client that validates one catalog entry learns the real number instead of
	// falling back to its own default.
	contextWindows := h.observedModelContextWindows(ctx)
	input, output := h.modelContextWindow(ctx, contextWindows, mChannel, m.ModelID, normalizeRequestedModelID(id))
	resp.ContextLength = input
	resp.MaxInputTokens = input
	resp.MaxOutputTokens = output

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		apperrors.New("api_error", "Failed to encode response", http.StatusInternalServerError).WriteResponse(w)
	}
}
