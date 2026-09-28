// Package pricing holds the official xAI price table and the pure functions the
// gateway uses to price a finished request and to reserve a budget before one
// starts. It is deliberately dependency-free: the same numbers must be usable
// from the request path, the audit writer and tests.
//
// Money is carried as integer USD ticks (1 USD = 10,000,000,000 ticks) so a
// ledger can accumulate costs without floating point drift. The published rates
// are per 1M tokens, which is 1e6 times the per-token tick value used here.
package pricing

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

const (
	// Source is the official page the table below was transcribed from.
	Source = "https://docs.x.ai/developers/pricing"
	// AsOf is the date the published rates were captured.
	AsOf = "2026-08-13"
	// Version identifies the table revision. It is persisted on every priced
	// ledger/audit row so an old row can always be read against the rates that
	// produced it.
	Version = "official-2026-08-13"
	// TicksPerUSD is the fixed point scale of every cost in this package.
	TicksPerUSD int64 = 10_000_000_000
)

// Result is one priced request: the canonical model the rate was resolved to and
// its cost in USD ticks. An empty Result with ok == false means "unpriced",
// which callers must keep distinguishable from a genuine zero cost.
type Result struct {
	Model          string
	CostInUSDTicks int64
}

// tokenPrice is one row of the official text table. The *Ticks values are
// integer USD ticks per token; LongContextTokens is the input size above which
// the long-context column applies.
type tokenPrice struct {
	CanonicalModel    string
	InputTicks        int64
	CachedInputTicks  int64
	OutputTicks       int64
	LongContextTokens int64
	LongInputTicks    int64
	LongCachedTicks   int64
	LongOutputTicks   int64
}

var officialTokenPrices = buildOfficialTokenPrices()

// tokenPriceRule resolves a model family when the exact name is not published:
// the official page prices every suffix of a family at the family rate.
type tokenPriceRule struct {
	Pattern        *regexp.Regexp
	CanonicalModel string
}

// officialTokenPriceRules keeps the two special shapes (non-reasoning and
// multi-agent) ahead of the generic family pattern, so a specific suffix is
// never priced as the reasoning default.
var officialTokenPriceRules = []tokenPriceRule{
	{Pattern: regexp.MustCompile(`^grok-(?:build-0\.1|code-fast(?:-1)?|composer-2\.5-fast)(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-build-0.1"},
	{Pattern: regexp.MustCompile(`^grok-4\.6(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.6"},
	{Pattern: regexp.MustCompile(`^grok-4\.5(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.5"},
	{Pattern: regexp.MustCompile(`^grok-4\.3(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.3"},
	{Pattern: regexp.MustCompile(`^grok-4\.20-multi-agent(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.20-multi-agent-0309"},
	{Pattern: regexp.MustCompile(`^grok-4\.20(?:-[a-z0-9.]+)*-non-reasoning(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.20-0309-non-reasoning"},
	{Pattern: regexp.MustCompile(`^grok-4\.20(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.20-0309-reasoning"},
}

// buildOfficialTokenPrices returns the official per-token rate table. Every
// alias resolves to the same canonical row.
//
// Published rates (USD per 1M tokens, standard / long-context above 200k input):
//
//	grok-build-0.1              $1     / $0.20 cached / $2     -> $2 / $0.40 / $4
//	grok-4.6                    $2     / $0.50 cached / $6     -> $4 / $1    / $12
//	grok-4.5                    $2     / $0.30 cached / $6     -> $4 / $0.60 / $12
//	grok-4.3 and 4.20 family    $1.25  / $0.20 cached / $2.50  -> $2.50 / $0.40 / $5
func buildOfficialTokenPrices() map[string]tokenPrice {
	prices := make(map[string]tokenPrice)
	register := func(canonical string, price tokenPrice, names ...string) {
		price.CanonicalModel = canonical
		prices[canonical] = price
		for _, name := range names {
			prices[name] = price
		}
	}
	register("grok-build-0.1", tokenPrice{InputTicks: 10000, CachedInputTicks: 2000, OutputTicks: 20000, LongContextTokens: 200000, LongInputTicks: 20000, LongCachedTicks: 4000, LongOutputTicks: 40000},
		"grok-code-fast-1", "grok-code-fast", "grok-code-fast-1-0825", "grok-composer-2.5-fast")
	register("grok-4.6", tokenPrice{InputTicks: 20000, CachedInputTicks: 5000, OutputTicks: 60000, LongContextTokens: 200000, LongInputTicks: 40000, LongCachedTicks: 10000, LongOutputTicks: 120000},
		"grok-4.6-latest")
	register("grok-4.5", tokenPrice{InputTicks: 20000, CachedInputTicks: 3000, OutputTicks: 60000, LongContextTokens: 200000, LongInputTicks: 40000, LongCachedTicks: 6000, LongOutputTicks: 120000},
		"grok-4.5-latest", "grok-build-latest")
	standard := tokenPrice{InputTicks: 12500, CachedInputTicks: 2000, OutputTicks: 25000, LongContextTokens: 200000, LongInputTicks: 25000, LongCachedTicks: 4000, LongOutputTicks: 50000}
	register("grok-4.3", standard, "grok-4.3-latest", "grok-latest")
	register("grok-4.20-multi-agent-0309", standard,
		"grok-4.20-multi-agent", "grok-4.20-multi-agent-latest", "grok-4.20-multi-agent-beta-latest", "grok-4.20-multi-agent-beta-0309")
	register("grok-4.20-0309-reasoning", standard,
		"grok-4.20-reasoning-latest", "grok-4.20", "grok-4.20-reasoning", "grok-4.20-0309", "grok-4.20-beta", "grok-4.20-beta-0309", "grok-4.20-beta-latest", "grok-4.20-beta-reasoning", "grok-4.20-beta-latest-reasoning")
	register("grok-4.20-0309-non-reasoning", standard,
		"grok-4.20-non-reasoning", "grok-4.20-non-reasoning-latest", "grok-4.20-beta-non-reasoning", "grok-4.20-beta-latest-non-reasoning")
	return prices
}

// resolveOfficialTokenPrice handles the internal source prefixes and the exact
// published aliases first, then the anchored family rules. An unpriced model
// must never be treated as costing zero.
func resolveOfficialTokenPrice(model string) (tokenPrice, bool) {
	normalized := normalizePricingModel(model)
	if price, ok := officialTokenPrices[normalized]; ok {
		return price, true
	}
	for _, rule := range officialTokenPriceRules {
		if !rule.Pattern.MatchString(normalized) {
			continue
		}
		price, ok := officialTokenPrices[rule.CanonicalModel]
		return price, ok
	}
	return tokenPrice{}, false
}

// normalizePricingModel strips only the source prefixes the gateway itself
// attaches, so an arbitrary path fragment is never mistaken for a billable model.
func normalizePricingModel(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"build/", "grok_build/"} {
		if strings.HasPrefix(normalized, prefix) {
			return strings.TrimSpace(normalized[len(prefix):])
		}
	}
	return normalized
}

// EstimateCost prices one finished text request from its token usage.
//
// cachedInputTokens is the cached part of inputTokens (clamped to [0, input]);
// contextInputTokens selects the long-context column and defaults to inputTokens
// when zero, which is the best available approximation when the caller only
// knows the billed input size. Unknown models return false and a zero Result.
func EstimateCost(model string, inputTokens, cachedInputTokens, outputTokens, contextInputTokens int64) (Result, bool) {
	price, ok := resolveOfficialTokenPrice(model)
	if !ok {
		return Result{}, false
	}
	inputPrice := price.InputTicks
	cachedPrice := price.CachedInputTicks
	outputPrice := price.OutputTicks
	contextTokens := contextInputTokens
	if contextTokens <= 0 {
		contextTokens = inputTokens
	}
	if price.LongContextTokens > 0 && contextTokens > price.LongContextTokens {
		inputPrice = price.LongInputTicks
		cachedPrice = price.LongCachedTicks
		outputPrice = price.LongOutputTicks
	}
	inputTokens = max(int64(0), inputTokens)
	cachedTokens := max(int64(0), min(cachedInputTokens, inputTokens))
	uncachedTokens := max(int64(0), inputTokens-cachedTokens)
	outputTokens = max(int64(0), outputTokens)
	return Result{
		Model:          price.CanonicalModel,
		CostInUSDTicks: uncachedTokens*inputPrice + cachedTokens*cachedPrice + outputTokens*outputPrice,
	}, true
}

// EstimateTextReservationFromBody scans a JSON request once to obtain its model,
// approximate input tokens and output cap. Unlike the legacy estimator it does
// not build a map[string]any tree and then parse the same body again.
func EstimateTextReservationFromBody(body []byte) (Result, bool) {
	model, inputTokens, outputTokens, _ := scanReservationJSON(body)
	if strings.TrimSpace(model) == "" {
		return Result{}, false
	}
	if _, priced := resolveOfficialTokenPrice(model); !priced {
		return Result{}, false
	}
	return EstimateCost(model, inputTokens, 0, outputTokens, inputTokens)
}

func scanReservationJSON(body []byte) (model string, inputTokens, outputTokens int64, ok bool) {
	const defaultOutputTokens int64 = 16_384
	const maximumOutputTokens int64 = 131_072
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var outputByPriority [3]int64
	var walk func(bool) (int64, error)
	walk = func(top bool) (int64, error) {
		token, err := decoder.Token()
		if err != nil {
			return 0, err
		}
		switch typed := token.(type) {
		case json.Delim:
			switch typed {
			case '{':
				var total int64
				for decoder.More() {
					keyToken, err := decoder.Token()
					if err != nil {
						return 0, err
					}
					key, _ := keyToken.(string)
					total += int64((len(key)+2)/3) + 1
					if top && key == "model" {
						var value string
						if err := decoder.Decode(&value); err != nil {
							return 0, err
						}
						model = strings.TrimSpace(value)
						total += max(1, int64((len(value)+2)/3))
						continue
					}
					priority := -1
					if top {
						switch key {
						case "max_output_tokens":
							priority = 0
						case "max_completion_tokens":
							priority = 1
						case "max_tokens":
							priority = 2
						}
					}
					if priority >= 0 {
						var number json.Number
						if err := decoder.Decode(&number); err != nil {
							return 0, err
						}
						if value, err := number.Int64(); err == nil && value > 0 {
							outputByPriority[priority] = min(value, maximumOutputTokens)
						}
						total++
						continue
					}
					child, err := walk(false)
					if err != nil {
						return 0, err
					}
					total += child
				}
				_, err = decoder.Token()
				return total, err
			case '[':
				var total int64
				for decoder.More() {
					child, err := walk(false)
					if err != nil {
						return 0, err
					}
					total += 1 + child
				}
				_, err = decoder.Token()
				return total, err
			}
		case string:
			trimmed := strings.TrimSpace(typed)
			if strings.HasPrefix(trimmed, "data:image/") || strings.HasPrefix(trimmed, "data:video/") {
				return 256, nil
			}
			return max(1, int64((len(typed)+2)/3)), nil
		case json.Number, float64, bool:
			return 1, nil
		case nil:
			return 0, nil
		}
		return 0, nil
	}
	tokens, err := walk(true)
	if err != nil {
		return model, max(256, int64((len(body)+2)/3)), defaultOutputTokens, false
	}
	outputTokens = defaultOutputTokens
	for _, value := range outputByPriority {
		if value > 0 {
			outputTokens = value
			break
		}
	}
	return model, max(256, tokens+128), outputTokens, true
}

// ── Media pricing (images, videos) ────────────────────────────────────────────
//
// Ported from grok2api's EstimateOfficialImageCost / EstimateOfficialImageEditCost
// / EstimateOfficialVideoCost. The image and video planes are billed per produced
// asset rather than per token, so they get their own estimators instead of a
// token table.

// ── Cost reconstruction (PricingBreakdown) ────────────────────────────────────
//
// grok2api exposes the rate components behind a stored cost so an operator can
// answer "why is this row this expensive" without re-deriving the formula. The
// stored row keeps the quantities (tokens, images, seconds) and the pricing
// model; this reconstructs the components that produced the number.

// ComponentKind names one line of a price.
type ComponentKind string

const (
	ComponentUncachedInput ComponentKind = "uncached_input"
	ComponentCachedInput   ComponentKind = "cached_input"
	ComponentOutput        ComponentKind = "output"
	ComponentInputImage    ComponentKind = "input_image"
	ComponentOutputImage   ComponentKind = "output_image"
	ComponentOutputSecond  ComponentKind = "output_second"
)

// Unit is what a component counts.
type Unit string

const (
	UnitToken  Unit = "token"
	UnitImage  Unit = "image"
	UnitSecond Unit = "second"
)

// Component is one priced line: quantity times unit price.
type Component struct {
	Kind                ComponentKind `json:"kind"`
	Unit                Unit          `json:"unit"`
	Quantity            int64         `json:"quantity"`
	UnitPriceInUSDTicks int64         `json:"unit_price_usd_ticks"`
	CostInUSDTicks      int64         `json:"cost_usd_ticks"`
}

// Breakdown is a reconstructed cost: the total plus the components that make it.
type Breakdown struct {
	Model          string      `json:"model"`
	CostInUSDTicks int64       `json:"cost_in_usd_ticks"`
	Components     []Component `json:"components"`
}

func (b *Breakdown) add(kind ComponentKind, unit Unit, quantity, unitPrice int64) {
	if quantity <= 0 || unitPrice <= 0 {
		return
	}
	cost := quantity * unitPrice
	b.Components = append(b.Components, Component{
		Kind: kind, Unit: unit, Quantity: quantity, UnitPriceInUSDTicks: unitPrice, CostInUSDTicks: cost,
	})
	b.CostInUSDTicks += cost
}

// Quantities is what a stored row kept about its request.
type Quantities struct {
	InputTokens      int64
	CachedTokens     int64
	OutputTokens     int64
	ContextTokens    int64
	InputImages      int64
	OutputImages     int64
	OutputSeconds    int64
	Characters       int64
	StreamingSeconds float64
}

// ReconstructBreakdown rebuilds the components behind a priced row. It returns
// false when the model is not priced or its quantities are unusable, which is the
// same "unpriced" verdict the estimators give.
func ReconstructBreakdown(model string, q Quantities) (Breakdown, bool) {
	normalized := normalizePricingModel(model)
	breakdown := Breakdown{Model: normalized}

	price, ok := resolveOfficialTokenPrice(normalized)
	if !ok {
		return Breakdown{}, false
	}
	contextTokens := q.ContextTokens
	if contextTokens <= 0 {
		contextTokens = q.InputTokens
	}
	inputPrice, cachedPrice, outputPrice := price.InputTicks, price.CachedInputTicks, price.OutputTicks
	if price.LongContextTokens > 0 && contextTokens > price.LongContextTokens {
		inputPrice, cachedPrice, outputPrice = price.LongInputTicks, price.LongCachedTicks, price.LongOutputTicks
	}
	cached := max(int64(0), min(q.CachedTokens, q.InputTokens))
	uncached := max(int64(0), q.InputTokens-cached)
	breakdown.add(ComponentUncachedInput, UnitToken, uncached, inputPrice)
	breakdown.add(ComponentCachedInput, UnitToken, cached, cachedPrice)
	breakdown.add(ComponentOutput, UnitToken, max(int64(0), q.OutputTokens), outputPrice)
	return breakdown, breakdown.CostInUSDTicks > 0
}
