// Package modelcatalog defines provider-neutral metadata observed from an
// upstream model catalog. Profiles are durable account capability state: they
// contain no credentials and may safely be copied, persisted, and aggregated.
package modelcatalog

import "strings"

// Profile is the useful model metadata advertised by an upstream catalog.
type Profile struct {
	Name                    string   `json:"name,omitempty"`
	Description             string   `json:"description,omitempty"`
	ModelID                 string   `json:"model_id"`
	ReasoningEfforts        []string `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort  string   `json:"default_reasoning_effort,omitempty"`
	SupportsReasoningEffort bool     `json:"supports_reasoning_effort,omitempty"`
	ContextWindow           int      `json:"context_window,omitempty"`
	MaxCompletionTokens     int      `json:"max_completion_tokens,omitempty"`
	SupportsBackendSearch   bool     `json:"supports_backend_search,omitempty"`
}

// Normalize trims identifiers, canonicalizes the reasoning menu, and drops a
// default which is not actually present in that menu.
func Normalize(profile Profile) Profile {
	profile.ModelID = strings.TrimSpace(profile.ModelID)
	seen := make(map[string]struct{}, len(profile.ReasoningEfforts))
	efforts := make([]string, 0, len(profile.ReasoningEfforts))
	for _, raw := range profile.ReasoningEfforts {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		efforts = append(efforts, value)
	}
	profile.ReasoningEfforts = efforts
	profile.DefaultReasoningEffort = strings.ToLower(strings.TrimSpace(profile.DefaultReasoningEffort))
	if _, ok := seen[profile.DefaultReasoningEffort]; !ok {
		profile.DefaultReasoningEffort = ""
	}
	if len(efforts) > 0 {
		profile.SupportsReasoningEffort = true
	}
	if profile.ContextWindow < 0 {
		profile.ContextWindow = 0
	}
	if profile.MaxCompletionTokens < 0 {
		profile.MaxCompletionTokens = 0
	}
	return profile
}

// CloneProfiles returns a deep copy suitable for handing across account/store
// boundaries.
func CloneProfiles(profiles []Profile) []Profile {
	if profiles == nil {
		return nil
	}
	out := make([]Profile, len(profiles))
	copy(out, profiles)
	for i := range out {
		out[i].ReasoningEfforts = append([]string(nil), profiles[i].ReasoningEfforts...)
	}
	return out
}

// ModelIDs projects an ordered, case-insensitively de-duplicated profile list.
func ModelIDs(profiles []Profile) []string {
	seen := make(map[string]struct{}, len(profiles))
	out := make([]string, 0, len(profiles))
	for _, raw := range profiles {
		id := strings.TrimSpace(raw.ModelID)
		key := strings.ToLower(id)
		if id == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, id)
	}
	return out
}

// Aggregate combines account-scoped profiles conservatively. A public route may
// select any eligible account, so it only advertises reasoning/search features
// shared by every observation and the smallest positive token budgets.
func Aggregate(catalogs ...[]Profile) []Profile {
	var out []Profile
	index := map[string]int{}
	for _, catalog := range catalogs {
		for _, raw := range catalog {
			profile := Normalize(raw)
			key := strings.ToLower(profile.ModelID)
			if key == "" {
				continue
			}
			at, exists := index[key]
			if !exists {
				index[key] = len(out)
				out = append(out, profile)
				continue
			}
			current := out[at]
			allowed := make(map[string]struct{}, len(profile.ReasoningEfforts))
			for _, effort := range profile.ReasoningEfforts {
				allowed[effort] = struct{}{}
			}
			shared := current.ReasoningEfforts[:0]
			for _, effort := range current.ReasoningEfforts {
				if _, ok := allowed[effort]; ok {
					shared = append(shared, effort)
				}
			}
			current.ReasoningEfforts = shared
			if current.DefaultReasoningEffort != profile.DefaultReasoningEffort {
				current.DefaultReasoningEffort = ""
			}
			current.SupportsReasoningEffort = current.SupportsReasoningEffort && profile.SupportsReasoningEffort
			current.SupportsBackendSearch = current.SupportsBackendSearch && profile.SupportsBackendSearch
			if profile.ContextWindow > 0 && (current.ContextWindow == 0 || profile.ContextWindow < current.ContextWindow) {
				current.ContextWindow = profile.ContextWindow
			}
			if profile.MaxCompletionTokens > 0 && (current.MaxCompletionTokens == 0 || profile.MaxCompletionTokens < current.MaxCompletionTokens) {
				current.MaxCompletionTokens = profile.MaxCompletionTokens
			}
			out[at] = Normalize(current)
		}
	}
	return CloneProfiles(out)
}
