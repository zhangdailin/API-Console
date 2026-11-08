package web

import (
	"strings"
)

// ModelSpec defines one public Web model and how it maps to grok.com fields.
type ModelSpec struct {
	ID            string
	Name          string
	UpstreamModel string
	ModelMode     string
	ModeID        string
	Tier          int
	PreferBest    bool
	IsImage       bool
	IsVideo       bool
	MediaAPIOnly  bool
	// AliasReasoningEffort is kept for request-shape compatibility; Web modes
	// have no effort aliases, so it is always empty.
	AliasReasoningEffort string
}

const (
	grokTierBasic = iota
	grokTierLite
	grokTierSuper
	grokTierHeavy
)

// SupportedModels is the Grok Web route table. grok.com publishes no model
// catalog, so this table is the gateway's contract for the Web plane. A row is
// published to the model list only after an active Web SSO account's quota
// read for that route succeeded (see QuotaModelFor and the model refresh).
var SupportedModels = []ModelSpec{
	{ID: "grok-chat-fast", Name: "Grok Chat Fast", UpstreamModel: "grok-chat-fast", ModelMode: "MODEL_MODE_FAST", ModeID: "fast", Tier: grokTierBasic},
	{ID: "grok-chat-auto", Name: "Grok Chat Auto", UpstreamModel: "grok-chat-auto", ModelMode: "MODEL_MODE_AUTO", ModeID: "auto", Tier: grokTierSuper},
	{ID: "grok-chat-expert", Name: "Grok Chat Expert", UpstreamModel: "grok-chat-expert", ModelMode: "MODEL_MODE_EXPERT", ModeID: "expert", Tier: grokTierSuper},
	{ID: "grok-chat-heavy", Name: "Grok Chat Heavy", UpstreamModel: "grok-chat-heavy", ModelMode: "MODEL_MODE_HEAVY", ModeID: "heavy", Tier: grokTierHeavy, PreferBest: true},
	{ID: "grok-imagine-image", Name: "Grok Imagine Image", UpstreamModel: "grok-imagine-image", ModelMode: "MODEL_MODE_AUTO", ModeID: "auto", Tier: grokTierSuper, IsImage: true},
	{ID: "grok-imagine-image-2.0", Name: "Grok Imagine Image 2.0", UpstreamModel: "grok-imagine-image-2.0", ModelMode: "MODEL_MODE_AUTO", ModeID: "auto", Tier: grokTierSuper, IsImage: true},
	{ID: "grok-imagine-image-edit", Name: "Grok Imagine Image Edit", UpstreamModel: "imagine-image-edit", ModelMode: "MODEL_MODE_AUTO", ModeID: "auto", Tier: grokTierSuper, IsImage: true},
	{ID: "grok-imagine-video", Name: "Grok Imagine Video", UpstreamModel: "imagine-video-gen", ModelMode: "MODEL_MODE_AUTO", ModeID: "auto", Tier: grokTierSuper, IsVideo: true},
}

func (m ModelSpec) SupportsConversation() bool {
	return !m.MediaAPIOnly
}

var modelByID = func() map[string]ModelSpec {
	out := make(map[string]ModelSpec, len(SupportedModels))
	for _, m := range SupportedModels {
		out[strings.ToLower(strings.TrimSpace(m.ID))] = m
	}
	return out
}()

// IsDeprecatedModelID is kept for call-site compatibility: the Web table holds
// no retired identifiers.
func IsDeprecatedModelID(string) bool { return false }

func normalizeModelID(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

// stripProviderPublicPrefix removes the "web/" qualifier, reporting whether it
// removed anything.
func stripProviderPublicPrefix(id string) (string, bool) {
	normalized := normalizeModelID(id)
	if rest, ok := strings.CutPrefix(normalized, "web/"); ok {
		return strings.TrimSpace(rest), true
	}
	return normalized, false
}

func ResolveModelAlias(modelID string) (ModelSpec, string, bool) {
	id, _ := stripProviderPublicPrefix(modelID)
	m, ok := modelByID[id]
	return m, "", ok
}

func ResolveModel(modelID string) (ModelSpec, bool) {
	m, _, ok := ResolveModelAlias(modelID)
	return m, ok
}

// IsWebModelID reports whether a public model identifier names a Grok Web
// route. The grok channel uses it to dispatch a request to this plane.
func IsWebModelID(modelID string) bool {
	_, ok := ResolveModel(modelID)
	return ok
}

func (m ModelSpec) PoolCandidates() []string {
	switch {
	case m.PreferBest && m.Tier == grokTierHeavy:
		return []string{"heavy", "basic"}
	case m.PreferBest:
		return []string{"heavy", "super", "lite", "basic"}
	case m.Tier == grokTierHeavy:
		return []string{"heavy", "basic"}
	case m.Tier == grokTierSuper:
		return []string{"super", "lite", "heavy", "basic"}
	case m.Tier == grokTierLite:
		return []string{"lite", "super", "heavy", "basic"}
	default:
		return []string{"basic", "lite", "super", "heavy"}
	}
}
