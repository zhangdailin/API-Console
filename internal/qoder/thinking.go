package qoder

import (
	"bytes"
	"strings"

	"encoding/json"
)

// A catalog row's thinking_config is what the official client reads to decide
// whether a model thinks by default and which effort tiers it accepts:
//
//	{"disabled":{...},
//	 "enabled":{"is_default":true,
//	            "efforts":{"low":{},"medium":{"is_default":true},"xhigh":{}}}}
//
// The gateway does not reject an effort the model lacks; it ignores it and
// falls back to the model default, so a client asking qfmodel for "high" saw
// no effect and no error. Efforts are therefore mapped onto the model's own
// table before they are sent.

// effortRank orders the effort vocabulary the gateway uses.
var effortRank = map[string]int{"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// effortOrder is effortRank's keys in rank order.
var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// thinkingOff are the spellings a client uses to switch thinking off; the
// gateway's own value is "none".
var thinkingOff = map[string]bool{"none": true, "off": true, "disabled": true, "disable": true, "false": true, "0": true, "no": true}

type thinkingMode struct {
	IsDefault *bool                      `json:"is_default"`
	Efforts   map[string]json.RawMessage `json:"efforts"`
}

type thinkingConfig struct {
	Enabled  *thinkingMode `json:"enabled"`
	Disabled *thinkingMode `json:"disabled"`
}

// thinkingConfig decodes the row's block. ok is false when the row carries no
// block, or one this channel cannot read.
func (m modelEntry) thinkingConfig() (thinkingConfig, bool) {
	raw := bytes.TrimSpace(m.ThinkingConfig)
	if len(raw) == 0 || string(raw) == "null" {
		return thinkingConfig{}, false
	}
	var config thinkingConfig
	if json.Unmarshal(raw, &config) != nil {
		return thinkingConfig{}, false
	}
	return config, true
}

// reasoningDefault reports whether the model thinks when the caller sent no
// reasoning effort. The modes' own is_default flags decide; without them an
// enabled mode means thinking. A missing block falls back to is_reasoning, and
// an unreadable one cannot rule thinking out. The gateway ships rows whose
// is_reasoning contradicts the block (false beside a default-on enabled mode).
func (m modelEntry) reasoningDefault() bool {
	raw := bytes.TrimSpace(m.ThinkingConfig)
	if len(raw) == 0 || string(raw) == "null" {
		return m.IsReasoning
	}
	config, ok := m.thinkingConfig()
	if !ok {
		return true
	}
	switch {
	case config.Enabled != nil && config.Enabled.IsDefault != nil && *config.Enabled.IsDefault:
		return true
	case config.Disabled != nil && config.Disabled.IsDefault != nil && *config.Disabled.IsDefault:
		return false
	}
	return config.Enabled != nil
}

// supportedEfforts lists the model's effort tiers in rank order and its
// default tier. An empty list with ok means the model only turns thinking on
// or off. ok is false when the row declares no thinking block at all.
func (m modelEntry) supportedEfforts() (efforts []string, defaultEffort string, ok bool) {
	config, ok := m.thinkingConfig()
	if !ok {
		return nil, "", false
	}
	if config.Enabled == nil {
		return nil, "", true
	}
	for _, effort := range effortOrder {
		raw, present := config.Enabled.Efforts[effort]
		if !present {
			continue
		}
		efforts = append(efforts, effort)
		var tier struct {
			IsDefault bool `json:"is_default"`
		}
		if json.Unmarshal(raw, &tier) == nil && tier.IsDefault {
			defaultEffort = effort
		}
	}
	if defaultEffort == "" && len(efforts) > 0 {
		defaultEffort = efforts[len(efforts)/2]
	}
	return efforts, defaultEffort, true
}

// normalizeEffort maps a requested effort onto what the model accepts.
//
//   - any off spelling becomes "none";
//   - a model without a thinking block gets the request verbatim;
//   - a model with only on/off gets no effort ("" with thinking left on);
//   - a supported tier passes; an unsupported one becomes the nearest tier,
//     ties going toward the model default; an unknown word becomes the default.
func (m modelEntry) normalizeEffort(requested string) string {
	effort := strings.ToLower(strings.TrimSpace(requested))
	if effort == "" {
		return ""
	}
	if thinkingOff[effort] {
		return "none"
	}
	efforts, defaultEffort, ok := m.supportedEfforts()
	if !ok {
		return effort
	}
	if len(efforts) == 0 {
		return ""
	}
	want, known := effortRank[effort]
	if !known {
		return defaultEffort
	}
	best := ""
	for _, candidate := range efforts {
		if candidate == effort {
			return effort
		}
		if best == "" || closerEffort(candidate, best, want, defaultEffort) {
			best = candidate
		}
	}
	return best
}

func closerEffort(candidate, best string, want int, defaultEffort string) bool {
	distance := func(effort string) int { return abs(effortRank[effort] - want) }
	if distance(candidate) != distance(best) {
		return distance(candidate) < distance(best)
	}
	pull := effortRank[defaultEffort]
	return abs(effortRank[candidate]-pull) < abs(effortRank[best]-pull)
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
