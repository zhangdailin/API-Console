package responses

import (
	"io"
	"time"

	"orchids-api/internal/secureblob"
)

// Exported facade for the compaction machinery. The gateway runs a compaction
// turn itself so the resulting state stays portable across accounts, and both
// the native Build path and the chat bridge need the same codec and stream
// writer.

// CompactionCodec seals and opens a compaction blob.
type CompactionCodec = gatewayCompactionCodec

// NewCompactionCodec returns a codec for the given cipher.
func NewCompactionCodec(cipher *secureblob.Cipher) *CompactionCodec {
	return newGatewayCompactionCodec(cipher)
}

// CompactionKind classifies a compaction turn.
type CompactionKind = gatewayCompactionKind

const (
	CompactionNone    = responsesCompactionNone
	CompactionTrigger = responsesCompactionTrigger
	CompactionTUI     = responsesCompactionTUI
)

// CompactionMaxAttempts and CompactionRetryPause bound the retry loop around a
// summary turn.
const (
	CompactionMaxAttempts = gatewayCompactionMaxAttempts
)

// ClassifyCompactionPayload decides whether a payload is a compaction turn.
func ClassifyCompactionPayload(payload map[string]any) CompactionKind {
	return classifyResponsesCompactionPayload(payload)
}

// PrepareCompactionSample strips the compaction controls out of a payload.
func PrepareCompactionSample(payload map[string]any) map[string]any {
	return prepareGatewayCompactionSample(payload)
}

// ExpandCompactionHistory reopens sealed blobs in a replayed history.
func ExpandCompactionHistory(payload map[string]any, codec *CompactionCodec, session string) (int, error) {
	return expandGatewayCompactionHistory(payload, codec, session)
}

// BuildCompactionResponse renders the compaction result object.
func BuildCompactionResponse(response map[string]any, blob, model string) map[string]any {
	return buildGatewayCompactionResponse(response, blob, model)
}

// WriteCompactionStream emits the synthetic SSE sequence a compaction client expects.
func WriteCompactionStream(w io.Writer, response map[string]any) error {
	return writeGatewayCompactionStream(w, response)
}

// CompactionRandomHex mints the random half of a synthetic response id.
func CompactionRandomHex(size int) string { return compactionRandomHex(size) }

// ExtractCompactionSummary reads the summary text out of a completed response.
func ExtractCompactionSummary(response map[string]any) string {
	return extractCompactionSummary(response)
}

// CompactionErrorIsTransient reports whether a summary-turn failure is retryable.
func CompactionErrorIsTransient(err error) bool { return gatewayCompactionErrorIsTransient(err) }

// CompactionBlobError is the typed failure of opening a sealed blob.
type CompactionBlobError = compactionBlobError

// IsDegenerateCompactionSummary reports whether a summary is unusable.
func IsDegenerateCompactionSummary(summary string) bool {
	return isDegenerateGatewayCompactionSummary(summary)
}

// CompactionContinuation returns the continuation prompt for a partial summary.
func CompactionContinuation(raw string) string { return gatewayCompactionContinuation(raw) }

// CompactionRetryPause is the delay between summary-turn attempts. It is a
// variable so a test can drive the retry loop without sleeping for seconds.
var CompactionRetryPause = gatewayCompactionRetryPause

// SetCompactionRetryPause overrides the retry pause and returns the previous
// value, for tests.
func SetCompactionRetryPause(d time.Duration) time.Duration {
	previous := gatewayCompactionRetryPause
	gatewayCompactionRetryPause = d
	return previous
}

// CompactionPrompt is the summary prompt sent for a gateway compaction turn.
var CompactionPrompt = gatewayCompactionPrompt

// Available reports whether the codec can seal and open blobs.
func (c *CompactionCodec) Available() bool { return c.available() }

// Encode seals a summary into a blob this gateway can reopen.
func (c *CompactionCodec) Encode(session, summary string) (string, error) {
	return c.encode(session, summary)
}

// Decode opens a blob, reporting whether this gateway owns it and whether the
// session key drifted.
func (c *CompactionCodec) Decode(session, blob string) (summary string, owned, sessionDrifted bool, err error) {
	return c.decode(session, blob)
}

// CompactionHTTPErrorIsTransient reports whether an HTTP failure is retryable.
func CompactionHTTPErrorIsTransient(status int, body string) bool {
	return compactionHTTPErrorIsTransient(status, body)
}

// ParseCompactionStream reads one SSE sample out of a summary stream.
func ParseCompactionStream(data []byte) (gatewayCompactionSample, error) {
	return parseGatewayCompactionStream(data)
}

// ErrCompactionDegenerate marks a summary the model could not produce.
var ErrCompactionDegenerate = errGatewayCompactionDegenerate

// NonNegativeJSONInteger reads a usage counter that must not be negative.
func NonNegativeJSONInteger(value any) int64 { return nonNegativeJSONInteger(value) }

// ClientCompactionPromptMarker is the distinctive line from the compaction
// prompt that identifies a TUI-style compaction turn.
const ClientCompactionPromptMarker = clientCompactionPromptMarker

// CompactionPrefix marks a blob this gateway sealed.
const CompactionPrefix = gatewayCompactionPrefix
