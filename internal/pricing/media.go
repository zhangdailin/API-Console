package pricing

import "strings"

// Media (image and video) prices for the Grok Web plane, from the same
// official table as the text rates.

// / EstimateOfficialVideoCost. The image and video planes are billed per produced
// asset rather than per token, so they get their own estimators instead of a
// token table.

const (
	officialImageEditInputTicks int64 = 100_000_000
	officialLiteImageInputTicks int64 = 20_000_000
)

// officialImage20OutputTicks is the resolution/quality matrix for 2.0.
func officialImage20OutputTicks(resolution, quality string) (int64, bool) {
	switch resolution + "/" + quality {
	case "1k/low":
		return 400_000_000, true
	case "2k/low", "1k/medium":
		return 600_000_000, true
	case "2k/medium":
		return 800_000_000, true
	default:
		return 0, false
	}
}

// EstimateImageEditCost prices an edit: every output image plus every input image
// the model had to process.
func EstimateImageEditCost(model, resolution, quality string, outputCount, inputCount int) (Result, bool) {
	model = normalizePricingModel(model)
	if outputCount <= 0 || inputCount <= 0 {
		return Result{}, false
	}
	resolution = strings.ToLower(strings.TrimSpace(resolution))
	quality = strings.ToLower(strings.TrimSpace(quality))
	if resolution == "" {
		resolution = "1k"
	}
	pricingModel := ""
	inputTicks := officialImageEditInputTicks
	var outputTicks int64
	switch model {
	case "grok-imagine-image-edit":
		if quality != "" {
			return Result{}, false
		}
		pricingModel = "grok-imagine-image-edit-" + resolution
	case "grok-imagine-image-2.0":
		if quality == "" {
			quality = "medium"
		}
		ticks, ok := officialImage20OutputTicks(resolution, quality)
		if !ok {
			return Result{}, false
		}
		outputTicks = ticks
		pricingModel = "grok-imagine-image-2.0-edit-" + quality + "-" + resolution
	case "grok-imagine-image-quality":
		if quality != "" {
			return Result{}, false
		}
		switch resolution {
		case "1k":
			outputTicks = 500_000_000
		case "2k":
			outputTicks = 700_000_000
		default:
			return Result{}, false
		}
		pricingModel = "grok-imagine-image-quality-" + resolution
	case "grok-imagine-image":
		if quality != "" {
			return Result{}, false
		}
		inputTicks = officialLiteImageInputTicks
		outputTicks = 200_000_000
		pricingModel = "grok-imagine-image"
	default:
		return Result{}, false
	}
	if outputTicks == 0 {
		switch resolution {
		case "1k":
			outputTicks = 500_000_000
		case "2k":
			outputTicks = 700_000_000
		default:
			return Result{}, false
		}
	}
	return Result{
		Model:          pricingModel,
		CostInUSDTicks: int64(outputCount)*outputTicks + int64(inputCount)*inputTicks,
	}, true
}

// EstimateVideoCost prices a generated video from its duration and resolution,
// plus the reference images the model consumed.
func EstimateVideoCost(model, resolution string, seconds, inputImages int) (Result, bool) {
	if seconds <= 0 || inputImages < 0 {
		return Result{}, false
	}
	baseModel := normalizePricingModel(model)
	if baseModel != "grok-imagine-video" && baseModel != "grok-imagine-video-1.5" {
		return Result{}, false
	}
	resolution = strings.ToLower(strings.TrimSpace(resolution))
	var ticksPerSecond, ticksPerInputImage int64
	switch baseModel {
	case "grok-imagine-video":
		ticksPerInputImage = officialLiteImageInputTicks
		switch resolution {
		case "480p":
			ticksPerSecond = 500_000_000
		case "720p":
			ticksPerSecond = 700_000_000
		default:
			return Result{}, false
		}
	case "grok-imagine-video-1.5":
		ticksPerInputImage = officialImageEditInputTicks
		switch resolution {
		case "480p":
			ticksPerSecond = 800_000_000
		case "720p":
			ticksPerSecond = 1_400_000_000
		case "1080p":
			ticksPerSecond = 2_500_000_000
		default:
			return Result{}, false
		}
	}
	return Result{
		Model:          baseModel + "-" + resolution,
		CostInUSDTicks: int64(seconds)*ticksPerSecond + int64(inputImages)*ticksPerInputImage,
	}, true
}

// ── Cost reconstruction (PricingBreakdown) ────────────────────────────────────
//
// grok2api exposes the rate components behind a stored cost so an operator can
// answer "why is this row this expensive" without re-deriving the formula. The
