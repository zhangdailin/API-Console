package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"encoding/json"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/modelpolicy"
)

// Codex-family clients call GET /v1/models?client_version=... and expect a
// catalog richer than the OpenAI list: per-model context window, input
// modalities, reasoning levels and truncation policy. A client that only sees
// id/object/created/owned_by falls back to its own defaults, which is what makes
// a capable model look text-only with a tiny context window.

const codexBaseInstructions = "You are Codex, a coding agent. Follow the user's instructions and use the available tools to complete software engineering tasks. Inspect relevant files before editing, preserve unrelated changes, and verify the result."

type codexReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

type codexTruncationPolicy struct {
	Mode  string `json:"mode"`
	Limit int    `json:"limit"`
}

type codexModelEntry struct {
	Slug                              string                `json:"slug"`
	DisplayName                       string                `json:"display_name"`
	Description                       string                `json:"description"`
	DefaultReasoningLevel             string                `json:"default_reasoning_level"`
	SupportedReasoningLevels          []codexReasoningLevel `json:"supported_reasoning_levels"`
	ShellType                         string                `json:"shell_type"`
	Visibility                        string                `json:"visibility"`
	MinimalClientVersion              string                `json:"minimal_client_version"`
	SupportedInAPI                    bool                  `json:"supported_in_api"`
	Priority                          int                   `json:"priority"`
	AdditionalSpeedTiers              []string              `json:"additional_speed_tiers"`
	ServiceTiers                      []any                 `json:"service_tiers"`
	DefaultServiceTier                *string               `json:"default_service_tier"`
	AvailabilityNUX                   any                   `json:"availability_nux"`
	Upgrade                           any                   `json:"upgrade"`
	BaseInstructions                  string                `json:"base_instructions"`
	ModelMessages                     any                   `json:"model_messages"`
	IncludeSkillsUsageInstructions    bool                  `json:"include_skills_usage_instructions"`
	SupportsReasoningSummaryParameter bool                  `json:"supports_reasoning_summary_parameter"`
	SupportsReasoningSummaries        bool                  `json:"supports_reasoning_summaries"`
	DefaultReasoningSummary           string                `json:"default_reasoning_summary"`
	SupportVerbosity                  bool                  `json:"support_verbosity"`
	DefaultVerbosity                  *string               `json:"default_verbosity"`
	ApplyPatchToolType                *string               `json:"apply_patch_tool_type"`
	WebSearchToolType                 string                `json:"web_search_tool_type"`
	TruncationPolicy                  codexTruncationPolicy `json:"truncation_policy"`
	SupportsParallelToolCalls         bool                  `json:"supports_parallel_tool_calls"`
	SupportsImageDetailOriginal       bool                  `json:"supports_image_detail_original"`
	ContextWindow                     int                   `json:"context_window"`
	MaxOutputTokens                   int                   `json:"max_output_tokens,omitempty"`
	MaxContextWindow                  int                   `json:"max_context_window"`
	EffectiveContextWindowPercent     int                   `json:"effective_context_window_percent"`
	AutoCompactTokenLimit             *int                  `json:"auto_compact_token_limit"`
	ExperimentalSupportedTools        []string              `json:"experimental_supported_tools"`
	InputModalities                   []string              `json:"input_modalities"`
	SupportsSearchTool                bool                  `json:"supports_search_tool"`
	UseResponsesLite                  bool                  `json:"use_responses_lite"`
}

type codexModelCatalog struct {
	Models []codexModelEntry `json:"models"`
}

var codexReasoningDescriptions = map[string]string{
	"none":    "No reasoning",
	"minimal": "Fastest responses with minimal reasoning",
	"low":     "Fast responses with lighter reasoning",
	"medium":  "Balances speed and reasoning depth for everyday tasks",
	"high":    "Greater reasoning depth for complex problems",
	"xhigh":   "Extra high reasoning depth for complex problems",
	"max":     "Maximum reasoning depth for the hardest problems",
}

type codexModelMetadata struct {
	contextWindow int
	description   string
	imageInput    bool
}

// Keyed by the upstream model slug, with provider prefixes already stripped.
var codexModelMetadataTable = map[string]codexModelMetadata{
	"grok-4.5":               {500000, "xAI Grok 4.5 frontier model with reasoning and vision.", true},
	"grok-4.6":               {500000, "xAI Grok 4.6 frontier model with reasoning and vision.", true},
	"grok-composer-2.5-fast": {200000, "xAI Grok Composer 2.5 model.", false},
}

// codexDefaultDescription is the single source for the unknown-model copy. The
// wording is pinned: clients match on this string, so changing it is a wire
// change even though nothing in the gateway parses it.
const codexDefaultDescription = "Grok model served via grok2api."

var codexDefaultMetadata = codexModelMetadata{
	contextWindow: 128000,
	description:   codexDefaultDescription,
}

// Reasoning levels and provider scoping live in modelpolicy so the wire
// normalization and the advertised catalog can never diverge.
func codexReasoningLevelsFor(publicID string) []string {
	return modelpolicy.SupportedReasoningEfforts(publicID)
}

// The static fallback prefers medium, then the first supported level.
func codexDefaultReasoningLevel(levels []string) string {
	for _, level := range levels {
		if level == "medium" {
			return level
		}
	}
	if len(levels) > 0 {
		return levels[0]
	}
	return "none"
}

func codexObservedReasoning(item PublicModelResponse) ([]string, string, bool) {
	if item.SupportsReasoningEffort == nil && len(item.ReasoningEfforts) == 0 && strings.TrimSpace(item.DefaultReasoningEffort) == "" {
		return nil, "", false
	}

	seen := make(map[string]struct{}, len(item.ReasoningEfforts))
	levels := make([]string, 0, len(item.ReasoningEfforts))
	for _, raw := range item.ReasoningEfforts {
		level := strings.ToLower(strings.TrimSpace(raw))
		if _, known := codexReasoningDescriptions[level]; !known {
			continue
		}
		if _, duplicate := seen[level]; duplicate {
			continue
		}
		seen[level] = struct{}{}
		levels = append(levels, level)
	}

	defaultLevel := strings.ToLower(strings.TrimSpace(item.DefaultReasoningEffort))
	if _, supported := seen[defaultLevel]; !supported {
		defaultLevel = ""
	}
	if defaultLevel == "" {
		defaultLevel = codexDefaultReasoningLevel(levels)
	}
	return levels, defaultLevel, true
}

func codexReasoningLevelEntries(levels []string) []codexReasoningLevel {
	result := make([]codexReasoningLevel, 0, len(levels))
	for _, level := range levels {
		result = append(result, codexReasoningLevel{Effort: level, Description: codexReasoningDescriptions[level]})
	}
	return result
}

func codexHasCapability(item PublicModelResponse, capability string) bool {
	return slices.ContainsFunc(item.Capabilities, func(value string) bool { return strings.EqualFold(strings.TrimSpace(value), capability) })
}

func codexAgentToolsSupported(item PublicModelResponse) bool {
	return strings.EqualFold(strings.TrimSpace(item.Provider), "build") && codexHasCapability(item, "responses")
}

// Media endpoints are not agent chat models, so they are listed but hidden.
func codexVisibilityFor(item PublicModelResponse) string {
	for _, capability := range []string{"image", "image_edit", "video"} {
		if codexHasCapability(item, capability) {
			return "hide"
		}
	}
	return "list"
}

func codexDisplayName(slug string) string {
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(slug))
	for index, word := range words {
		runes := []rune(word)
		if len(runes) > 0 {
			runes[0] = unicode.ToUpper(runes[0])
			words[index] = string(runes)
		}
	}
	return strings.Join(words, " ")
}

// codexEffortSuffixes are the effort variants collapsed into one
// reasoning-capable catalog entry. A catalog may publish gpt-5-6-sol-low,
// -medium, -high and -xhigh as separate models; presenting them as a single
// family with supported_reasoning_levels is what lets a client ask for the
// family plus an effort instead of guessing the exact suffix — and the gateway
// resolves that family name back onto the variant the catalog actually has.
var codexEffortSuffixes = []string{"low", "medium", "high", "xhigh", "max"}

// splitEffortVariantSuffix splits "<family>-<effort>" into its parts. It is the
// single definition of "this model id already names an effort variant", shared
// by the catalog grouping and by the request-path resolution so the two can
// never disagree about which ids are bare families.
func splitEffortVariantSuffix(modelID string) (family, level string) {
	for _, suffix := range codexEffortSuffixes {
		if strings.HasSuffix(modelID, "-"+suffix) {
			return strings.TrimSuffix(modelID, "-"+suffix), suffix
		}
	}
	return modelID, ""
}

func effortLevelRank(level string) int {
	for index, suffix := range codexEffortSuffixes {
		if suffix == level {
			return index
		}
	}
	return len(codexEffortSuffixes)
}

func sortEffortLevels(levels []string) {
	slices.SortStableFunc(levels, func(a, b string) int { return effortLevelRank(a) - effortLevelRank(b) })
}

func newCodexModelCatalog(items []PublicModelResponse) codexModelCatalog {
	type effortFamily struct {
		representative PublicModelResponse
		levels         []string
		variants       int
	}

	families := make(map[string]*effortFamily, len(items))
	order := make([]string, 0, len(items))
	for _, item := range items {
		name, level := splitEffortVariantSuffix(item.ID)
		if level == "" {
			// Not an effort variant: it stands alone under its exact id.
			families[item.ID] = &effortFamily{representative: item, variants: 1}
			order = append(order, item.ID)
			continue
		}
		entry, exists := families[name]
		if !exists {
			entry = &effortFamily{representative: item}
			families[name] = entry
			order = append(order, name)
		}
		entry.variants++
		// The representative decides visibility, capabilities and metadata, so
		// prefer a visible variant: a hidden "-low" must not mask the family.
		if codexVisibilityFor(entry.representative) != "list" && codexVisibilityFor(item) == "list" {
			entry.representative = item
		}
		if !slices.Contains(entry.levels, level) {
			entry.levels = append(entry.levels, level)
		}
	}

	models := make([]codexModelEntry, 0, len(order))
	for index, name := range order {
		entry := families[name]
		item := entry.representative
		// A lone "<family>-<effort>" id is not a family: collapsing it would
		// advertise a slug the store does not have while hiding the one it does.
		// Only a real set of variants becomes family + supported_reasoning_levels.
		if entry.variants < 2 {
			name = item.ID
			entry.levels = nil
		}
		slug := modelpolicy.GrokModelSlug(name)
		metadata, ok := codexModelMetadataTable[slug]
		if !ok {
			metadata = codexDefaultMetadata
		}
		// A window observed from the channel's own catalog beats both the static
		// table and the default. The default is Grok-shaped (128k), so applying it
		// to a Qoder or WorkBuddy model reported a 1M-token model as eight times
		// smaller and made the client compact a long session far too early.
		contextWindow := metadata.contextWindow
		if item.ContextLength > 0 {
			contextWindow = item.ContextLength
		} else if item.MaxInputTokens > 0 {
			contextWindow = item.MaxInputTokens
		}
		maxContextWindow := contextWindow
		if item.MaxOutputTokens > 0 {
			maxContextWindow += item.MaxOutputTokens
		}
		levels, defaultReasoningLevel, observedReasoning := codexObservedReasoning(item)
		if !observedReasoning {
			levels = entry.levels
			if len(levels) == 0 {
				levels = codexReasoningLevelsFor(name)
			} else {
				sortEffortLevels(levels)
			}
			defaultReasoningLevel = codexDefaultReasoningLevel(levels)
		}
		modalities := []string{"text"}
		if metadata.imageInput {
			modalities = append(modalities, "image")
		}
		// Agent tools require the Build Responses route.
		toolsSupported := codexAgentToolsSupported(item)
		reasoningSupported := false
		for _, level := range levels {
			if level != "none" {
				reasoningSupported = true
				break
			}
		}
		models = append(models, codexModelEntry{
			Slug:                              name,
			DisplayName:                       codexDisplayName(slug),
			Description:                       metadata.description,
			DefaultReasoningLevel:             defaultReasoningLevel,
			SupportedReasoningLevels:          codexReasoningLevelEntries(levels),
			ShellType:                         "shell_command",
			Visibility:                        codexVisibilityFor(item),
			MinimalClientVersion:              "0.0.0",
			SupportedInAPI:                    true,
			Priority:                          index + 1,
			AdditionalSpeedTiers:              []string{},
			ServiceTiers:                      []any{},
			BaseInstructions:                  codexBaseInstructions,
			IncludeSkillsUsageInstructions:    false,
			SupportsReasoningSummaryParameter: reasoningSupported,
			SupportsReasoningSummaries:        reasoningSupported,
			DefaultReasoningSummary:           "auto",
			SupportVerbosity:                  false,
			ApplyPatchToolType:                nil,
			WebSearchToolType:                 "text",
			TruncationPolicy:                  codexTruncationPolicy{Mode: "tokens", Limit: 10000},
			SupportsParallelToolCalls:         toolsSupported,
			SupportsImageDetailOriginal:       false,
			ContextWindow:                     contextWindow,
			MaxOutputTokens:                   item.MaxOutputTokens,
			MaxContextWindow:                  maxContextWindow,
			EffectiveContextWindowPercent:     95,
			ExperimentalSupportedTools:        []string{},
			InputModalities:                   modalities,
			SupportsSearchTool:                false,
			UseResponsesLite:                  false,
		})
	}
	return codexModelCatalog{Models: models}
}

func writeCodexModelCatalog(w http.ResponseWriter, r *http.Request, catalog codexModelCatalog) {
	body, err := json.Marshal(catalog)
	if err != nil {
		apperrors.New("api_error", "Failed to encode response", http.StatusInternalServerError).WriteResponse(w)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	if match := strings.TrimSpace(r.Header.Get("If-None-Match")); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
