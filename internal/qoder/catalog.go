package qoder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"encoding/json"
)

// The catalog is an upstream observation, not a compiled-in list.
//
// The active Qoder CLI path uses device-token refresh, user profile and a
// COSY-signed chat endpoint. The model catalog is read from that same signed
// control plane by FetchUpstreamModels, and the account snapshot records what
// the read returned.
//
//   - There is deliberately no built-in fallback. A compiled-in list is not an
//     observation of what the account may run: publishing it would advertise
//     models the account never had, and it would hide a gateway that stopped
//     answering the catalog read.
//   - With no snapshot, the pool skips the account. Direct client resolution
//     reports ErrNoUpstreamCatalog so the operator sees "refresh models".

// ErrNoUpstreamCatalog reports that no upstream catalog has been observed for
// the account. It is a state rather than a failure: the fix is a model refresh
// with an active account.
var ErrNoUpstreamCatalog = errors.New("qoder account has no upstream model catalog; refresh models with an active account")

// modelEntry is one catalog row. Key is the internal gateway key; Name is what a
// client may ask for.
type modelEntry struct {
	Key            string   `json:"key"`
	Name           string   `json:"name"`
	DisplayName    string   `json:"display_name"`
	Format         string   `json:"format"`
	Source         string   `json:"source"`
	Enable         *bool    `json:"enable"`
	IsVL           bool     `json:"is_vl"`
	IsReasoning    bool     `json:"is_reasoning"`
	IsDefault      bool     `json:"is_default"`
	PriceFactor    *float64 `json:"price_factor,omitempty"`
	MaxInputTokens int      `json:"max_input_tokens"`
	OrgTags        []string `json:"organization_tags"`
	// MaxOutputTokens and ThinkingConfig stay raw: the gateway has sent the cap
	// as a number and as a string, and one undecodable field must not fail the
	// whole catalog read. See defaultMaxOutputTokens and reasoningDefault.
	MaxOutputTokens json.RawMessage `json:"max_output_tokens,omitempty"`
	ThinkingConfig  json.RawMessage `json:"thinking_config,omitempty"`
	// Preserve opaque upstream tier data, including fields we cannot interpret.
	ContextConfig json.RawMessage `json:"context_config,omitempty"`
	// Snapshot-only sidecar: additional distinct context_config values observed
	// on duplicate enabled rows. Never replaces the canonical input budget.
	ContextConfigVariants []json.RawMessage `json:"context_config_variants,omitempty"`
}

// enabled reports whether the gateway currently serves this row. A missing flag
// means enabled: the field is an opt-out, and treating absence as disabled would
// hide every model from a deployment that omits it.
func (m modelEntry) enabled() bool { return m.Enable == nil || *m.Enable }

// fallbackMaxOutputTokens is the completion cap the official client uses when a
// catalog row carries no usable max_output_tokens.
const fallbackMaxOutputTokens = 32000

// defaultMaxOutputTokens is the completion cap sent when the caller set none.
// The official client reads it from the catalog row and falls back to 32000
// only for a missing or non-positive value; a fixed 32000 overshoots models
// whose cap is lower.
func (m modelEntry) defaultMaxOutputTokens() int {
	if tokens, ok := m.declaredMaxOutputTokens(); ok {
		return tokens
	}
	return fallbackMaxOutputTokens
}

// declaredMaxOutputTokens reads max_output_tokens as a positive safe integer,
// accepting the number and the string spelling. ok is false when the row
// declares no usable value.
func (m modelEntry) declaredMaxOutputTokens() (int, bool) {
	raw := bytes.TrimSpace(m.MaxOutputTokens)
	if len(raw) == 0 {
		return 0, false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		raw = []byte(strings.TrimSpace(text))
	}
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || value <= 0 || value > 1<<53-1 {
		return 0, false
	}
	return int(value), true
}

// Catalog is a resolved model catalog.
type Catalog struct {
	// entries are ordered as the upstream returned them.
	entries []modelEntry
	byKey   map[string]modelEntry
	byName  map[string]modelEntry
}

// Len reports how many models the catalog carries.
func (c *Catalog) Len() int {
	if c == nil {
		return 0
	}
	return len(c.entries)
}

// Entries returns the catalog rows in upstream order.
func (c *Catalog) Entries() []modelEntry {
	if c == nil {
		return nil
	}
	return append([]modelEntry(nil), c.entries...)
}

// FreeModelIDs returns only models the current upstream catalog explicitly
// prices at zero. A missing price_factor is unknown, not free; using a pointer
// preserves that distinction across the stored account snapshot.
func FreeModelIDs(ids []string) map[string]struct{} {
	catalog := catalogFromIDs(ids)
	if catalog == nil {
		return nil
	}
	out := make(map[string]struct{})
	for _, entry := range catalog.entries {
		for _, value := range []string{entry.Key, entry.Name, entry.DisplayName} {
			value = strings.ToLower(strings.TrimSpace(value))
			if value == "" {
				continue
			}
			resolved, err := catalog.Resolve(value)
			if err == nil && resolved.PriceFactor != nil && *resolved.PriceFactor == 0 {
				out[value] = struct{}{}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// IsFreeModel reports whether this account's latest observed catalog explicitly
// marks the requested model as zero-factor. No free row means no downgrade mode.
func IsFreeModel(ids []string, requested string) bool {
	_, ok := FreeModelIDs(ids)[strings.ToLower(strings.TrimSpace(requested))]
	return ok
}

// Resolve maps a client-facing model name onto a catalog row.
//
// Both the internal key and the display name are accepted. A client that copies
// a key out of this gateway's own /v1/models response must keep working, and
// operators reasonably paste either form.
func (c *Catalog) Resolve(requested string) (modelEntry, error) {
	if c == nil || len(c.entries) == 0 {
		// A compiled-in catalog is not what the account may run. Resolving
		// against it would accept a model the account never advertised, so an
		// unknown catalog is reported instead.
		return modelEntry{}, ErrNoUpstreamCatalog
	}
	name := strings.TrimSpace(requested)
	if name == "" {
		entry, ok := c.defaultEntry()
		if !ok {
			return modelEntry{}, ErrNoUpstreamCatalog
		}
		return entry, nil
	}
	// Indexes use the same normalization as public metadata. Keys always win
	// over aliases, including when the caller changes the spelling's case.
	name = strings.ToLower(name)
	if entry, ok := c.byKey[name]; ok {
		return entry, nil
	}
	if entry, ok := c.byName[name]; ok {
		return entry, nil
	}
	return modelEntry{}, fmt.Errorf("unsupported qoder model %q; available: %s", requested, c.SupportedList())
}

func (c *Catalog) defaultEntry() (modelEntry, bool) {
	if c == nil || len(c.entries) == 0 {
		return modelEntry{}, false
	}
	for _, entry := range c.entries {
		if entry.IsDefault {
			return entry, true
		}
	}
	// Prefer the cheapest capable tier rather than the flagship, so an empty
	// model name never silently routes every request to the most expensive one.
	for _, needle := range []string{"flash", "lite", "efficient", "plus"} {
		for _, entry := range c.entries {
			if strings.Contains(strings.ToLower(entry.Name), needle) {
				return entry, true
			}
		}
	}
	return c.entries[0], true
}

// SupportedList renders the catalog for an error message.
func (c *Catalog) SupportedList() string {
	if c == nil {
		return ""
	}
	names := make([]string, 0, len(c.entries))
	for _, entry := range c.entries {
		names = append(names, entry.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func newCatalog(entries []modelEntry) *Catalog {
	catalog := &Catalog{
		byKey:  make(map[string]modelEntry, len(entries)),
		byName: make(map[string]modelEntry, len(entries)),
	}
	positions := make(map[string]int, len(entries))
	for _, entry := range entries {
		if !entry.enabled() {
			continue
		}
		key := strings.TrimSpace(entry.Key)
		if key == "" || strings.EqualFold(key, "auto") {
			// `auto` is a routing directive, not a runnable model.
			continue
		}
		// Keep the first enabled observation's budget/capabilities. Duplicate
		// keys contribute their opaque context tiers, never a larger input budget.
		normalizedKey := strings.ToLower(key)
		if position, exists := positions[normalizedKey]; exists {
			mergeContextConfigs(&catalog.entries[position], entry)
			continue
		}
		entry.Key = key
		entry.Name = strings.TrimSpace(entry.Name)
		entry.DisplayName = strings.TrimSpace(entry.DisplayName)
		// The wire uses display_name; the stored snapshot and the in-process
		// form use name. Either may be absent, so the key is the last resort.
		if strings.TrimSpace(entry.Name) == "" {
			entry.Name = strings.TrimSpace(entry.DisplayName)
		}
		if strings.TrimSpace(entry.Name) == "" {
			entry.Name = key
		}
		if entry.Format == "" {
			entry.Format = "openai"
		}
		if entry.Source == "" {
			entry.Source = "system"
		}
		positions[normalizedKey] = len(catalog.entries)
		catalog.entries = append(catalog.entries, entry)
	}
	// Build indexes after merging so key and every alias see the same tiers.
	for _, entry := range catalog.entries {
		catalog.byKey[strings.ToLower(entry.Key)] = entry
		for _, alias := range []string{entry.Name, entry.DisplayName} {
			alias = strings.ToLower(alias)
			if alias == "" {
				continue
			}
			if _, taken := catalog.byName[alias]; !taken {
				catalog.byName[alias] = entry
			}
		}
	}
	return catalog
}

// catalogFromIDs rebuilds a catalog from an account's stored snapshot. The
// snapshot stores "<key>\t<display name>" so a display name that differs from
// the key round-trips, and a bare key (a hand-edited or imported snapshot) still
// resolves.
func catalogFromIDs(ids []string) *Catalog {
	entries := make([]modelEntry, 0, len(ids))
	for _, id := range ids {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		// The stored form is one JSON row per model, which is what preserves the
		// wire fields routing needs (max_input_tokens, is_reasoning, is_vl,
		// price_factor). A "<key>\t<display name>" row from an older deployment is
		// still accepted so an upgraded install keeps resolving its snapshot.
		if strings.HasPrefix(trimmed, "{") {
			var entry modelEntry
			if err := json.Unmarshal([]byte(trimmed), &entry); err != nil {
				continue
			}
			entries = append(entries, entry)
			continue
		}
		key, name, _ := strings.Cut(trimmed, "\t")
		if strings.TrimSpace(key) == "" {
			continue
		}
		entries = append(entries, modelEntry{Key: strings.TrimSpace(key), Name: strings.TrimSpace(name)})
	}
	return newCatalog(entries)
}

// catalogToIDs projects a catalog onto the stored snapshot form.
//
// Each row is stored as its own JSON object rather than the older
// "<key>\t<display name>" pair. Routing rebuilds the request's model block from
// this snapshot, and that block carries max_input_tokens, is_reasoning and is_vl;
// a two-field snapshot would silently drop them and the gateway would receive a
// request with no context length.
func catalogToIDs(catalog *Catalog) []string {
	if catalog == nil {
		return nil
	}
	ids := make([]string, 0, len(catalog.entries))
	for _, entry := range catalog.entries {
		if strings.TrimSpace(entry.Key) == "" {
			continue
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		ids = append(ids, string(raw))
	}
	return ids
}

// CatalogContextWindows publishes the input window each model is actually
// served with: the default context tier this channel sends as context_length,
// bounded by max_input_tokens. Publishing max_input_tokens alone advertised 1M
// for a model served at its 200K default tier, so a client trusting it kept
// growing a history the upstream then refused or cut. Every alias reports the
// row it actually resolves to.
func CatalogContextWindows(ids []string) map[string]int {
	return catalogWindows(ids, modelEntry.servedInputTokens)
}

// CatalogOutputWindows publishes max_output_tokens where the catalog states
// one. The 32000 fallback is this channel's default, not an observation, so it
// is never published.
func CatalogOutputWindows(ids []string) map[string]int {
	return catalogWindows(ids, modelEntry.declaredOutputTokens)
}

func catalogWindows(ids []string, measure func(modelEntry) int) map[string]int {
	catalog := catalogFromIDs(ids)
	if catalog == nil || catalog.Len() == 0 {
		return nil
	}
	out := make(map[string]int, catalog.Len()*3)
	for _, entry := range catalog.entries {
		for _, name := range []string{entry.Key, entry.Name, entry.DisplayName} {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			// An ambiguous display name must advertise the row that actually
			// resolves, not the largest budget among unrelated products.
			resolved, err := catalog.Resolve(name)
			if err != nil {
				continue
			}
			if tokens := measure(resolved); tokens > 0 {
				out[name] = tokens
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// servedContextTokens is the context_length a request for this model carries:
// the default tier, or max_input_tokens when no tier is declared.
func (m modelEntry) servedContextTokens() int {
	if tier := m.ContextWindowInfo().DefaultContextTokens; tier > 0 {
		return tier
	}
	return m.MaxInputTokens
}

// servedInputTokens is the input a request for this model can carry: the
// served context, never more than max_input_tokens.
func (m modelEntry) servedInputTokens() int {
	served := m.servedContextTokens()
	if m.MaxInputTokens > 0 && (served <= 0 || m.MaxInputTokens < served) {
		return m.MaxInputTokens
	}
	return served
}

func (m modelEntry) declaredOutputTokens() int {
	tokens, _ := m.declaredMaxOutputTokens()
	return tokens
}

// FetchModels returns the catalog already observed for this account.
// It is a pure read of the account snapshot: the catalog is written by
// FetchUpstreamModels during a refresh, and a chat request must not perform
// catalog I/O. With no snapshot there is nothing to resolve against, so the
// read reports that instead of substituting a compiled-in list.
func (c *Client) FetchModels(ctx context.Context) (*Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("qoder client is nil")
	}
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	if c.account != nil {
		if catalog := catalogFromIDs(c.account.QoderModelIDs); catalog.Len() > 0 {
			return catalog, nil
		}
	}
	return nil, ErrNoUpstreamCatalog
}
