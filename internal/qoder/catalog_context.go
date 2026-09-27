package qoder

import (
	"bytes"
	"strings"

	"github.com/goccy/go-json"
)

// ContextWindowInfo separates a default input budget from declared context
// tiers. MaxContextTokens is only the largest explicitly understood tier, NOT
// a verified model limit or permission to use that tier. Zero means unknown.
// The observed gateway shape is a label-keyed object whose tier values contain
// token_count and optional boolean is_default (e.g. 200K, 400K, 1M, 272K).
// Labels are opaque: their K/M spelling is never parsed as a token count.
// Explicit context_length objects/arrays are also understood conservatively;
// unknown nested schemas and fields remain preserved in the raw observation.
type ContextWindowInfo struct {
	DefaultInputTokens   int  `json:"default_input_tokens,omitempty"`
	DefaultContextTokens int  `json:"default_context_tokens,omitempty"`
	MaxContextTokens     int  `json:"max_context_tokens,omitempty"`
	DefaultConflict      bool `json:"default_conflict,omitempty"`
	HasUnparsedConfig    bool `json:"has_unparsed_config,omitempty"`
}

func mergeContextConfigs(dst *modelEntry, src modelEntry) {
	seen := make(map[string]bool)
	for _, raw := range append([]json.RawMessage{dst.ContextConfig}, dst.ContextConfigVariants...) {
		seen[compactContextConfig(raw)] = true
	}
	for _, raw := range append([]json.RawMessage{src.ContextConfig}, src.ContextConfigVariants...) {
		key := compactContextConfig(raw)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		copied := append(json.RawMessage(nil), raw...)
		if len(dst.ContextConfig) == 0 {
			dst.ContextConfig = copied
		} else {
			dst.ContextConfigVariants = append(dst.ContextConfigVariants, copied)
		}
	}
}

func compactContextConfig(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) == nil {
		return buf.String()
	}
	return string(raw)
}

// ContextWindowInfo summarizes only explicitly supported fields. Conflicting
// default declarations stay ambiguous instead of selecting the largest value.
func (m modelEntry) ContextWindowInfo() ContextWindowInfo {
	info := ContextWindowInfo{DefaultInputTokens: m.MaxInputTokens}
	var visit func(json.RawMessage)
	visit = func(raw json.RawMessage) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			return
		}
		if raw[0] == '[' {
			var rows []json.RawMessage
			if json.Unmarshal(raw, &rows) != nil {
				info.HasUnparsedConfig = true
				return
			}
			for _, row := range rows {
				visit(row)
			}
			return
		}
		var fields map[string]json.RawMessage
		if raw[0] != '{' || json.Unmarshal(raw, &fields) != nil {
			info.HasUnparsedConfig = true
			return
		}
		lengthRaw, explicit := fields["context_length"]
		if !explicit {
			lengthRaw, explicit = fields["token_count"]
		}
		if !explicit {
			// Observed label map; only immediate tier objects with token_count
			// are understood. Do not recursively guess arbitrary wrappers.
			for _, tier := range fields {
				var tierFields map[string]json.RawMessage
				if json.Unmarshal(tier, &tierFields) != nil || tierFields["token_count"] == nil {
					info.HasUnparsedConfig = true
					continue
				}
				visit(tier)
			}
			return
		}
		var length int
		if json.Unmarshal(lengthRaw, &length) != nil || length <= 0 {
			info.HasUnparsedConfig = true
			return
		}
		if length > info.MaxContextTokens {
			info.MaxContextTokens = length
		}
		var isDefault bool
		if value, ok := fields["is_default"]; ok {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &isDefault) != nil {
				info.HasUnparsedConfig = true
				return
			}
		}
		if isDefault {
			if info.DefaultContextTokens != 0 && info.DefaultContextTokens != length {
				info.DefaultConflict = true
			}
			info.DefaultContextTokens = length
		}
	}
	visit(m.ContextConfig)
	for _, raw := range m.ContextConfigVariants {
		visit(raw)
	}
	if info.DefaultConflict {
		info.DefaultContextTokens = 0
	}
	return info
}

// CatalogContextWindowDetails exposes tier metadata separately from the legacy
// CatalogContextWindows input-budget projection. Ambiguous names follow Resolve.
func CatalogContextWindowDetails(ids []string) map[string]ContextWindowInfo {
	catalog := catalogFromIDs(ids)
	if catalog.Len() == 0 {
		return nil
	}
	out := make(map[string]ContextWindowInfo)
	for _, entry := range catalog.entries {
		for _, name := range []string{entry.Key, entry.Name, entry.DisplayName} {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			resolved, err := catalog.Resolve(name)
			if err == nil {
				out[name] = resolved.ContextWindowInfo()
			}
		}
	}
	return out
}
