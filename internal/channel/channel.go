// Package channel defines provider identity and mechanical metadata shared by
// routing, account validation, model refresh and the admin UI. Provider-specific
// wire behavior remains in its own package; membership/order/prefixes do not.
package channel

import "strings"

type ID string

const (
	WorkBuddy ID = "workbuddy"
	Cline     ID = "cline"
	Grok      ID = "grok"
	Doubao    ID = "doubao"
)

type Definition struct {
	ID            ID     `json:"key"`
	Label         string `json:"label"`
	APIPrefix     string `json:"apiPrefix"`
	Generic       bool   `json:"generic"`
	Default       bool   `json:"default,omitempty"`
	Theme         string `json:"theme"`
	AccountCreate string `json:"accountCreate"`
	// ChatOnly restricts the channel to POST /chat/completions. Channels whose
	// upstream speaks only the OpenAI-compatible chat wire format leave
	// /messages and the Responses bridge unregistered.
	ChatOnly bool `json:"chatOnly,omitempty"`
	// FlowVisible controls whether the live traffic screen lists the channel.
	FlowVisible bool `json:"flowVisible"`
}

var definitions = [...]Definition{
	{ID: WorkBuddy, Label: "WorkBuddy", APIPrefix: "/workbuddy/v1", Generic: true, Default: true, Theme: "orange", AccountCreate: "browser", FlowVisible: true},
	{ID: Cline, Label: "Cline", APIPrefix: "/cline/v1", Generic: true, Theme: "blue", AccountCreate: "browser", FlowVisible: true},
	{ID: Grok, Label: "Grok", APIPrefix: "/grok/v1", Theme: "red", AccountCreate: "browser", FlowVisible: true},
	{ID: Doubao, Label: "Doubao", APIPrefix: "/doubao/v1", Generic: true, Theme: "green", AccountCreate: "browser", ChatOnly: true, FlowVisible: false},
}

func All() []Definition { return append([]Definition(nil), definitions[:]...) }

func Parse(value string) (ID, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, definition := range definitions {
		if value == string(definition.ID) || strings.EqualFold(value, definition.Label) {
			return definition.ID, true
		}
	}
	return "", false
}

func IsSupported(value string) bool { _, ok := Parse(value); return ok }

func DefinitionFor(id ID) (Definition, bool) {
	for _, definition := range definitions {
		if definition.ID == id {
			return definition, true
		}
	}
	return Definition{}, false
}

func Default() Definition {
	for _, definition := range definitions {
		if definition.Default {
			return definition
		}
	}
	return definitions[0]
}

func GenericPrefixes() []string {
	out := []string{}
	for _, definition := range definitions {
		if definition.Generic {
			out = append(out, PrefixesFor(definition.ID)...)
		}
	}
	return out
}

func AllPrefixes() []string {
	out := make([]string, 0, len(definitions)*2)
	for _, definition := range definitions {
		out = append(out, PrefixesFor(definition.ID)...)
	}
	return out
}

// ChatOnlyIDs returns the channels limited to POST /chat/completions.
func ChatOnlyIDs() []ID {
	out := make([]ID, 0, len(definitions))
	for _, definition := range definitions {
		if definition.ChatOnly {
			out = append(out, definition.ID)
		}
	}
	return out
}

// GenericPrefixesExcept returns the generic bases without the excluded channels.
func GenericPrefixesExcept(excluded ...ID) []string {
	skip := make(map[ID]struct{}, len(excluded))
	for _, id := range excluded {
		skip[id] = struct{}{}
	}
	out := []string{}
	for _, definition := range definitions {
		if !definition.Generic {
			continue
		}
		if _, found := skip[definition.ID]; found {
			continue
		}
		out = append(out, PrefixesFor(definition.ID)...)
	}
	return out
}

// FlowDefinitions returns the channels the live traffic screen renders.
func FlowDefinitions() []Definition {
	out := make([]Definition, 0, len(definitions))
	for _, definition := range definitions {
		if definition.FlowVisible {
			out = append(out, definition)
		}
	}
	return out
}

// FlowVisible reports whether the live traffic screen renders this channel.
// Unknown names are not flow channels.
func FlowVisible(value ID) bool {
	definition, ok := DefinitionFor(value)
	return ok && definition.FlowVisible
}

// PrefixesFor returns the versioned and unversioned inference base paths.
// APIPrefix remains the versioned URL advertised to OpenAI clients.
func PrefixesFor(id ID) []string {
	definition, ok := DefinitionFor(id)
	if !ok {
		return nil
	}
	return []string{definition.APIPrefix, strings.TrimSuffix(definition.APIPrefix, "/v1")}
}

func FromPath(path string) (ID, bool) {
	id, _, ok := EndpointFromPath(path)
	return id, ok
}

// EndpointFromPath requires a complete provider segment and strips either base.
func EndpointFromPath(path string) (ID, string, bool) {
	for _, definition := range definitions {
		for _, prefix := range PrefixesFor(definition.ID) {
			if path == prefix {
				return definition.ID, "", true
			}
			if endpoint, ok := strings.CutPrefix(path, prefix+"/"); ok {
				return definition.ID, endpoint, true
			}
		}
	}
	return "", "", false
}

func TrimModelPath(path string) (ID, string, bool) {
	id, endpoint, ok := EndpointFromPath(path)
	if ok {
		if model, matched := strings.CutPrefix(endpoint, "models/"); matched {
			return id, model, true
		}
	}
	return "", "", false
}
