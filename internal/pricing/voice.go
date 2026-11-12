package pricing

import (
	"math"
	"unicode/utf8"
)

// Official xAI voice prices (https://docs.x.ai/developers/pricing), in USD
// ticks (1 USD = 10,000,000,000 ticks).
const (
	// TTS is $15 per 1M characters.
	ttsCharacterTicks int64 = 150_000
	// STT is $0.10 per hour over REST and $0.20 per hour streaming.
	sttRESTHourTicks      int64 = 1_000_000_000
	sttStreamingHourTicks int64 = 2_000_000_000
)

// EstimateTTSCost prices speech synthesis from the Unicode characters sent.
func EstimateTTSCost(text string) (Result, bool) {
	characters := utf8.RuneCountInString(text)
	if characters <= 0 {
		return Result{}, false
	}
	return Result{Model: "grok-voice-tts", CostInUSDTicks: int64(characters) * ttsCharacterTicks}, true
}

// EstimateSTTCost prices a transcription from the audio duration the upstream
// reported. A positive duration never rounds down to zero.
func EstimateSTTCost(durationSeconds float64, streaming bool) (Result, bool) {
	if durationSeconds <= 0 || math.IsNaN(durationSeconds) || math.IsInf(durationSeconds, 0) {
		return Result{}, false
	}
	model, hourly := "grok-stt-rest", sttRESTHourTicks
	if streaming {
		model, hourly = "grok-stt-streaming", sttStreamingHourTicks
	}
	cost := int64(math.Ceil(durationSeconds * float64(hourly) / 3600))
	return Result{Model: model, CostInUSDTicks: max(1, cost)}, true
}
