package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// buildModelCatalogEntry accepts both the current snake_case Build response and
// older camelCase response variants.
type buildModelCatalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ID          string `json:"id"`
	Model       string `json:"model"`
	ModelID     string `json:"modelId"`
	Hidden      bool   `json:"hidden"`
	Meta        struct {
		Model   string `json:"model"`
		ModelID string `json:"modelId"`
		Hidden  bool   `json:"hidden"`
	} `json:"_meta"`
	ContextWindow            int64             `json:"context_window"`
	ContextWindowCamel       int64             `json:"contextWindow"`
	MaxCompletionTokens      int64             `json:"max_completion_tokens"`
	MaxCompletionTokensCamel int64             `json:"maxCompletionTokens"`
	ReasoningEffort          string            `json:"reasoning_effort"`
	ReasoningEffortCamel     string            `json:"reasoningEffort"`
	SupportsReasoningEffort  *bool             `json:"supports_reasoning_effort"`
	SupportsReasoningCamel   *bool             `json:"supportsReasoningEffort"`
	SupportsBackendSearch    bool              `json:"supports_backend_search"`
	ReasoningEfforts         []json.RawMessage `json:"reasoning_efforts"`
	ReasoningEffortsCamel    []json.RawMessage `json:"reasoningEfforts"`
}

func (e buildModelCatalogEntry) profile() modelcatalog.Profile {
	if e.Hidden || e.Meta.Hidden {
		return modelcatalog.Profile{}
	}
	profile := modelcatalog.Profile{Name: e.Name, Description: e.Description, ModelID: util.FirstNonEmpty(e.ID, e.Model, e.ModelID, e.Meta.Model, e.Meta.ModelID), SupportsBackendSearch: e.SupportsBackendSearch}
	if e.ContextWindow > 0 {
		profile.ContextWindow = clampCatalogInt(e.ContextWindow)
	} else {
		profile.ContextWindow = clampCatalogInt(e.ContextWindowCamel)
	}
	if e.MaxCompletionTokens > 0 {
		profile.MaxCompletionTokens = clampCatalogInt(e.MaxCompletionTokens)
	} else {
		profile.MaxCompletionTokens = clampCatalogInt(e.MaxCompletionTokensCamel)
	}
	if e.SupportsReasoningEffort != nil {
		profile.SupportsReasoningEffort = *e.SupportsReasoningEffort
	} else if e.SupportsReasoningCamel != nil {
		profile.SupportsReasoningEffort = *e.SupportsReasoningCamel
	}
	rawMenu := e.ReasoningEfforts
	if len(rawMenu) == 0 {
		rawMenu = e.ReasoningEffortsCamel
	}
	for _, raw := range rawMenu {
		var bare string
		if json.Unmarshal(raw, &bare) == nil && strings.TrimSpace(bare) != "" {
			profile.ReasoningEfforts = append(profile.ReasoningEfforts, bare)
			continue
		}
		var option struct {
			Value   string `json:"value"`
			Default bool   `json:"default"`
		}
		if json.Unmarshal(raw, &option) != nil || strings.TrimSpace(option.Value) == "" {
			continue
		}
		profile.ReasoningEfforts = append(profile.ReasoningEfforts, option.Value)
		if option.Default && profile.DefaultReasoningEffort == "" {
			profile.DefaultReasoningEffort = option.Value
		}
	}
	if profile.DefaultReasoningEffort == "" {
		profile.DefaultReasoningEffort = util.FirstNonEmpty(e.ReasoningEffort, e.ReasoningEffortCamel)
	}
	if e.SupportsReasoningEffort != nil && !*e.SupportsReasoningEffort || e.SupportsReasoningCamel != nil && e.SupportsReasoningEffort == nil && !*e.SupportsReasoningCamel {
		profile.ReasoningEfforts = nil
		profile.DefaultReasoningEffort = ""
	}
	return modelcatalog.Normalize(profile)
}

func clampCatalogInt(value int64) int {
	if value <= 0 {
		return 0
	}
	if value > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(value)
}

// FetchModelCatalog returns the complete account-scoped Build catalog profile.
func (c *CLIClient) FetchModelCatalog(ctx context.Context, acc *store.Account) ([]modelcatalog.Profile, error) {
	if c == nil || c.oauth == nil || acc == nil {
		return nil, fmt.Errorf("grok cli models is not configured")
	}
	ApplyCLIOAuthIdentity(acc)
	resp, err := c.request(ctx, acc, http.MethodGet, c.baseURL()+"/models", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, cliOAuthMaxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, newCLIUpstreamError(resp.StatusCode, resp.Header, body)
	}
	var payload struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode grok cli models response: %w", err)
	}
	profiles := make([]modelcatalog.Profile, 0, len(payload.Data))
	seen := make(map[string]struct{}, len(payload.Data))
	for _, raw := range payload.Data {
		var item buildModelCatalogEntry
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		profile := item.profile()
		key := strings.ToLower(profile.ModelID)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		profiles = append(profiles, profile)
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("grok cli models response contains no model ids")
	}
	return profiles, nil
}
