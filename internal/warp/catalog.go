package warp

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

const catalogSource = "warp_feature_model_choice"

// contextWindow is a direct field of the official LlmInfo choice (not nested
// under spec, which only carries cost/quality/speed). Each of the five feature
// planes is an execution profile of the same account catalog.
const catalogChoiceSelection = `id disableReason usageMetadata { creditMultiplier requestMultiplier } contextWindow { default max min isConfigurable }`
const catalogQuery = `query GetFeatureModelChoices($requestContext: RequestContext!) { user(requestContext: $requestContext) { __typename ... on UserOutput { user { workspaces { featureModelChoice { agentMode { choices { ` + catalogChoiceSelection + ` } } planning { choices { ` + catalogChoiceSelection + ` } } coding { choices { ` + catalogChoiceSelection + ` } } cliAgent { choices { ` + catalogChoiceSelection + ` } } computerUseAgent { choices { ` + catalogChoiceSelection + ` } } } } } } } }`

// ModelChoice carries usage and context budget observed in a single account's
// current catalog. Nil multipliers are unknown; request multipliers are legacy
// upstream metadata, not a substitute for credit multipliers or an estimate of
// gateway billing. A zero ContextWindow is unknown and is never defaulted.
type ModelChoice struct {
	ID                string
	CreditMultiplier  *float64
	RequestMultiplier *float64
	ContextWindow     int
}

// catalogPlane is one feature plane of a workspace's featureModelChoice.
type catalogPlane struct {
	Choices []catalogChoice `json:"choices"`
}

type catalogChoice struct {
	ID       string          `json:"id"`
	Disabled json.RawMessage `json:"disableReason"`
	Usage    json.RawMessage `json:"usageMetadata"`
	// contextWindow is a direct LlmInfo field in the official schema; it is not
	// inside spec (which only holds cost/quality/speed scalars).
	ContextWindow struct {
		Default        json.RawMessage `json:"default"`
		Max            json.RawMessage `json:"max"`
		Min            json.RawMessage `json:"min"`
		IsConfigurable json.RawMessage `json:"isConfigurable"`
	} `json:"contextWindow"`
}

type catalogFeatureModelChoice struct {
	AgentMode        catalogPlane `json:"agentMode"`
	Planning         catalogPlane `json:"planning"`
	Coding           catalogPlane `json:"coding"`
	CLIAgent         catalogPlane `json:"cliAgent"`
	ComputerUseAgent catalogPlane `json:"computerUseAgent"`
}

// planes returns every execution profile the catalog reports. The CLI-agent and
// computer-use planes are normal profiles even though model IDs that name them
// are still rejected by validModel.
func (f catalogFeatureModelChoice) planes() []catalogPlane {
	return []catalogPlane{f.AgentMode, f.Planning, f.Coding, f.CLIAgent, f.ComputerUseAgent}
}

func parseCatalogMultiplier(raw json.RawMessage) *float64 {
	var value float64
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" || json.Unmarshal(raw, &value) != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return nil
	}
	return &value
}

func parseCatalogUsage(raw json.RawMessage) (credit, request *float64) {
	var usage struct {
		Credit  json.RawMessage `json:"creditMultiplier"`
		Request json.RawMessage `json:"requestMultiplier"`
	}
	// Decode usage independently so malformed metadata cannot reject model IDs.
	if json.Unmarshal(raw, &usage) != nil {
		return nil, nil
	}
	return parseCatalogMultiplier(usage.Credit), parseCatalogMultiplier(usage.Request)
}

func equalCatalogMultiplier(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// parseCatalogTokenCount reads a token count without guessing at shapes the
// upstream did not send: fractional, non-numeric, non-positive and non-finite
// values are unknown rather than rounded or clamped.
func parseCatalogTokenCount(raw json.RawMessage) int {
	var value float64
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return 0
	}
	if json.Unmarshal(raw, &value) != nil || math.IsNaN(value) || math.IsInf(value, 0) ||
		value <= 0 || value != math.Trunc(value) || value > float64(math.MaxInt32) {
		return 0
	}
	return int(value)
}

// parseCatalogContextWindow follows the upstream semantics: the configured
// default when it is positive, otherwise the max the model permits. min and
// isConfigurable are observed fields, not window estimates.
func parseCatalogContextWindow(choice catalogChoice) int {
	if window := parseCatalogTokenCount(choice.ContextWindow.Default); window > 0 {
		return window
	}
	return parseCatalogTokenCount(choice.ContextWindow.Max)
}

var modelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)

func validModel(id string) bool {
	if !modelID.MatchString(id) || strings.Contains(strings.ToLower(id), "cli-agent") || strings.Contains(strings.ToLower(id), "computer-use-agent") {
		return false
	}
	for _, part := range strings.FieldsFunc(strings.ToLower(id), func(r rune) bool { return r == '/' || r == ':' || r == '-' || r == '_' || r == '.' }) {
		if part == "auto" || part == "all" {
			return false
		}
	}
	return true
}
func (c *Client) FetchModels(ctx context.Context) ([]string, error) {
	choices, err := c.FetchModelChoices(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(choices))
	for _, choice := range choices {
		ids = append(ids, choice.ID)
	}
	return ids, nil
}

// FetchModelChoices returns only active exact model IDs and their fresh metadata.
func (c *Client) FetchModelChoices(ctx context.Context) ([]ModelChoice, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]any{"query": catalogQuery, "operationName": "GetFeatureModelChoices", "variables": map[string]any{"requestContext": map[string]any{"clientContext": map[string]any{"version": clientVersion}, "osContext": map[string]any{"category": "Linux", "name": "Linux", "version": "", "linuxKernelVersion": nil}}}})
	var d struct {
		Data struct {
			User struct {
				Type string `json:"__typename"`
				User struct {
					Workspaces []struct {
						Feature catalogFeatureModelChoice `json:"featureModelChoice"`
					} `json:"workspaces"`
				} `json:"user"`
			} `json:"user"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := c.json(ctx, c.base+"/graphql/v2?op=GetFeatureModelChoices", "application/json", payload, token, &d); err != nil {
		return nil, err
	}
	if len(d.Errors) > 0 || d.Data.User.Type != "UserOutput" {
		return nil, errResponse
	}
	seen := map[string]int{}
	conflicting := map[string]bool{}
	windowConflicting := map[string]bool{}
	var choices []ModelChoice
	for _, workspace := range d.Data.User.User.Workspaces {
		for _, plane := range workspace.Feature.planes() {
			for _, choice := range plane.Choices {
				disabled := strings.TrimSpace(string(choice.Disabled))
				if !validModel(choice.ID) || (disabled != "" && disabled != "null" && disabled != `""`) {
					continue
				}
				credit, request := parseCatalogUsage(choice.Usage)
				window := parseCatalogContextWindow(choice)
				index, exists := seen[choice.ID]
				if !exists {
					seen[choice.ID] = len(choices)
					choices = append(choices, ModelChoice{
						ID: choice.ID, CreditMultiplier: credit, RequestMultiplier: request, ContextWindow: window,
					})
					continue
				}
				previous := &choices[index]
				if conflicting[choice.ID] || !equalCatalogMultiplier(previous.CreditMultiplier, credit) || !equalCatalogMultiplier(previous.RequestMultiplier, request) {
					// Workspace and plane disagreements are not a range across
					// accounts. Do not let whichever one happens to come first
					// supply the value.
					previous.CreditMultiplier, previous.RequestMultiplier = nil, nil
					conflicting[choice.ID] = true
				}
				// Planes are separate views of one catalog, so two different
				// positive windows are unresolved evidence rather than a
				// precedence rule. A plane that omits the window is not evidence
				// against one that reports it.
				switch {
				case windowConflicting[choice.ID] || previous.ContextWindow == window:
				case previous.ContextWindow == 0:
					previous.ContextWindow = window
				case window != 0:
					previous.ContextWindow = 0
					windowConflicting[choice.ID] = true
				}
			}
		}
	}
	if len(choices) == 0 {
		return nil, errResponse
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].ID < choices[j].ID })
	return choices, nil
}

// observeCatalog keeps the observation bound to the credential generation that
// authenticated it. FetchModelChoices may rotate credentials on the client's
// private account; copying only those fields avoids replaying stale operator state.
func (c *Client) observeCatalog(ctx context.Context) (provider.Observation, error) {
	choices, err := c.FetchModelChoices(ctx)
	if err != nil {
		return provider.Observation{}, err
	}
	c.mu.Lock()
	grant := credentials(c.account)
	c.mu.Unlock()
	ids := make([]string, 0, len(choices))
	windows := make(map[string]int, len(choices))
	for _, choice := range choices {
		ids = append(ids, choice.ID)
		if choice.ContextWindow > 0 {
			windows[choice.ID] = choice.ContextWindow
		}
	}
	models := catalogModels(ids)
	observedAt := time.Now().UTC()
	for i, choice := range choices {
		if choice.CreditMultiplier == nil && choice.RequestMultiplier == nil {
			continue
		}
		models[i].UpstreamUsage = &store.ModelUsageMetadata{
			CreditMultiplierMin: choice.CreditMultiplier, CreditMultiplierMax: choice.CreditMultiplier,
			RequestMultiplierMin: choice.RequestMultiplier, RequestMultiplierMax: choice.RequestMultiplier,
			AccountCount: 1, Source: catalogSource, ObservedAt: observedAt,
		}
	}
	return provider.Observation{
		Models: models,
		Record: func(acc *store.Account, now time.Time) bool {
			if acc == nil || len(ids) == 0 {
				return false
			}
			// Rotation has already been committed by accessToken. Do not request
			// credential replacement when this snapshot is persisted later.
			applyCredentials(acc, grant)
			setCatalogSnapshotWithWindows(acc, ids, windows, now)
			return true
		},
	}, nil
}

// setCatalogSnapshot records a catalog identity list without window evidence.
// It is the shape the login flow has: a fresh list and no contextWindow read.
func setCatalogSnapshot(acc *store.Account, ids []string, now time.Time) {
	setCatalogSnapshotWithWindows(acc, ids, nil, now)
}

// setCatalogSnapshotWithWindows records one catalog observation. The windows
// map is replaced, never merged with the previous generation: a model that
// disappeared from the catalog must not keep an old budget, and an observation
// without windows must not inherit them.
func setCatalogSnapshotWithWindows(acc *store.Account, ids []string, windows map[string]int, now time.Time) {
	acc.WarpModelIDs = append([]string(nil), ids...)
	acc.WarpModelsSyncedAt = now
	acc.WarpModelWindows = catalogWindows(windows)
}

// catalogWindows keeps only positive windows of IDs this channel can publish.
// An absent window is unknown, so it is omitted rather than defaulted.
func catalogWindows(windows map[string]int) map[string]int {
	if len(windows) == 0 {
		return nil
	}
	observed := make(map[string]int, len(windows))
	for id, window := range windows {
		if validModel(id) && window > 0 {
			observed[id] = window
		}
	}
	if len(observed) == 0 {
		return nil
	}
	return observed
}
func catalogModels(ids []string) []provider.Model {
	out := make([]provider.Model, 0, len(ids))
	for _, id := range ids {
		if validModel(id) {
			out = append(out, provider.Model{ID: id, Name: id, UpstreamModel: id, Provider: "warp"})
		}
	}
	return out
}
func catalogPort() *provider.Catalog {
	return &provider.Catalog{Source: catalogSource, RequestTimeoutSeconds: 20,
		Eligible: func(acc *store.Account) bool { return acc != nil && acc.WarpRefreshToken != "" },
		Observe: func(ctx context.Context, env provider.Env, acc *store.Account) (provider.Observation, error) {
			c := newClientWithStore(acc, env.Config, env.Store)
			return c.observeCatalog(ctx)
		},
		// A failed upstream read must not publish cached rows as observations.
		LastKnown:   func(*store.Account) []provider.Model { return nil },
		SetSnapshot: setCatalogSnapshot,
		Persist: func(ctx context.Context, s *store.Store, acc *store.Account) error {
			return s.UpdateWarpCatalogSnapshot(ctx, acc.ID, store.WarpCatalogPatch{
				ExpectedRefreshToken: acc.WarpRefreshToken,
				ModelIDs:             acc.WarpModelIDs,
				ModelWindows:         acc.WarpModelWindows,
				SyncedAt:             acc.WarpModelsSyncedAt,
			})
		},
		ShapeRecord: func(m *store.Model) { m.Capabilities = []string{"chat", "text"} },
	}
}
func freshSnapshot(acc *store.Account) bool {
	return acc != nil && !acc.WarpModelsSyncedAt.IsZero() && time.Since(acc.WarpModelsSyncedAt) >= 0 && time.Since(acc.WarpModelsSyncedAt) < provider.HealthRefreshInterval
}
