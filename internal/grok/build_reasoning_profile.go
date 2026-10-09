package grok

import (
	"fmt"
	"slices"
	"strings"

	"encoding/json"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/store"
)

// buildReasoningProfileError is a request validation failure discovered only
// after Build account selection. Catalog capabilities are account-scoped, so
// validating before selection would either reject a valid route or silently
// send an unsupported effort to the selected credential.
type buildReasoningProfileError struct {
	model     string
	effort    string
	supported []string
}

func (e *buildReasoningProfileError) Error() string {
	return fmt.Sprintf("reasoning.effort %q is not supported by model %s on the selected Build account; supported values: %s", e.effort, e.model, strings.Join(e.supported, ", "))
}

// cloneBuildPayload makes an attempt-local payload. Retry normalization may
// edit nested reasoning state, so a shallow map copy is not sufficient.
func cloneBuildPayload(payload map[string]interface{}) (map[string]interface{}, error) {
	if payload == nil {
		return map[string]interface{}{}, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var cloned map[string]interface{}
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

func buildCatalogProfile(acc *store.Account, upstreamModel string) (modelcatalog.Profile, bool) {
	if acc == nil {
		return modelcatalog.Profile{}, false
	}
	for _, raw := range acc.GrokModelCatalog {
		if strings.EqualFold(strings.TrimSpace(raw.ModelID), strings.TrimSpace(upstreamModel)) {
			return modelcatalog.Normalize(raw), true
		}
	}
	return modelcatalog.Profile{}, false
}

// buildPayloadForAccount derives an attempt payload from immutable request
// input, then applies the selected account's catalog profile. When old account
// records have no profile, the static compatibility policy remains the fallback.
func buildPayloadForAccount(immutable map[string]interface{}, acc *store.Account, upstreamModel string) (map[string]interface{}, error) {
	payload, err := cloneBuildPayload(immutable)
	if err != nil {
		return nil, err
	}
	profile, found := buildCatalogProfile(acc, upstreamModel)
	if !found {
		reasoning, _ := payload["reasoning"].(map[string]interface{})
		explicit := reasoning != nil && strings.TrimSpace(chatwire.ParseLooseStringAny(reasoning["effort"])) != ""
		if explicit && strings.EqualFold(strings.TrimSpace(chatwire.ParseLooseStringAny(reasoning["effort"])), "none") &&
			!modelpolicy.SupportsReasoningEffort(upstreamModel, "none") && modelpolicy.SupportsReasoningEffort(upstreamModel, "low") {
			reasoning["effort"] = "low"
		}
		if !explicit && modelpolicy.SupportsReasoningEffort(upstreamModel, "low") {
			if reasoning == nil {
				reasoning = map[string]interface{}{}
			}
			reasoning["effort"] = "low"
			payload["reasoning"] = reasoning
		}
		normalizeBuildReasoningEffort(payload, upstreamModel)
		return payload, nil
	}

	reasoning, _ := payload["reasoning"].(map[string]interface{})
	explicit := reasoning != nil && strings.TrimSpace(chatwire.ParseLooseStringAny(reasoning["effort"])) != ""
	if !explicit {
		if profile.SupportsReasoningEffort {
			if reasoning == nil {
				reasoning = map[string]interface{}{}
			}
			if effortSupported(profile.ReasoningEfforts, "low") {
				reasoning["effort"] = "low"
			} else if profile.DefaultReasoningEffort != "" {
				reasoning["effort"] = profile.DefaultReasoningEffort
			}
			payload["reasoning"] = reasoning
		}
		return payload, nil
	}

	effort := strings.ToLower(strings.TrimSpace(chatwire.ParseLooseStringAny(reasoning["effort"])))
	allowed := make(map[string]struct{}, len(profile.ReasoningEfforts))
	for _, candidate := range profile.ReasoningEfforts {
		allowed[strings.ToLower(strings.TrimSpace(candidate))] = struct{}{}
	}
	accept := func(candidate string) bool {
		_, ok := allowed[candidate]
		return ok
	}

	normalized := effort
	if !accept(normalized) {
		switch effort {
		case "none":
			for _, candidate := range []string{"low", "medium", "high", "xhigh"} {
				if accept(candidate) {
					normalized = candidate
					break
				}
			}
		case "minimal":
			if accept("low") {
				normalized = "low"
			}
		case "max":
			if accept("xhigh") {
				normalized = "xhigh"
			} else if accept("high") {
				normalized = "high"
			}
		}
	}
	if !profile.SupportsReasoningEffort || !accept(normalized) {
		return nil, &buildReasoningProfileError{model: upstreamModel, effort: effort, supported: profile.ReasoningEfforts}
	}
	reasoning["effort"] = normalized
	payload["reasoning"] = reasoning
	return payload, nil
}

func effortSupported(efforts []string, target string) bool {
	return slices.ContainsFunc(efforts, func(effort string) bool { return strings.EqualFold(strings.TrimSpace(effort), target) })
}

func replacePayload(dst map[string]interface{}, src map[string]interface{}) {
	clear(dst)
	for key, value := range src {
		dst[key] = value
	}
}

// prepareBuildPayload applies account capabilities without changing the source
// shared by retries and returns the payload used by both Chat and Responses.
func prepareBuildPayload(source, payload map[string]interface{}, acc *store.Account, model string) error {
	attempt, err := buildPayloadForAccount(source, acc, model)
	if err != nil {
		return err
	}
	replacePayload(payload, attempt)
	return nil
}
