// Package channel defines provider identity and mechanical metadata shared by
// routing, account validation, model refresh and the admin UI. Provider-specific
// wire behavior remains in its own package; membership/order/prefixes do not.
package channel

import "strings"

type ID string

const (
	WorkBuddy ID = "workbuddy"
	Qoder     ID = "qoder"
	Cline     ID = "cline"
	Grok      ID = "grok"
)

type Definition struct {
	ID            ID     `json:"key"`
	Label         string `json:"label"`
	APIPrefix     string `json:"apiPrefix"`
	Generic       bool   `json:"generic"`
	Default       bool   `json:"default,omitempty"`
	Theme         string `json:"theme"`
	AccountCreate string `json:"accountCreate"`
}

var definitions = [...]Definition{
	{ID: WorkBuddy, Label: "WorkBuddy", APIPrefix: "/workbuddy/v1", Generic: true, Default: true, Theme: "orange", AccountCreate: "browser"},
	{ID: Qoder, Label: "Qoder", APIPrefix: "/qoder/v1", Generic: true, Theme: "green", AccountCreate: "browser"},
	{ID: Cline, Label: "Cline", APIPrefix: "/cline/v1", Generic: true, Theme: "blue", AccountCreate: "browser"},
	{ID: Grok, Label: "Grok", APIPrefix: "/grok/v1", Theme: "red", AccountCreate: "browser"},
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
