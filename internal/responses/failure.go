package responses

import (
	"fmt"

	"orchids-api/internal/util"
)

// Failure reads the error out of a Responses event or object.
//
// The upstream puts the failure in `error`, or on the nested `response` object,
// or flat on `message`; all three are read so a caller never answers with the
// generic fallback when the upstream did say why it failed.
func Failure(ev map[string]any) error {
	value := ev["error"]
	if response, ok := ev["response"].(map[string]any); ok {
		value = response["error"]
	}
	// detail is a nil map when the upstream error is not an object; reading it is
	// safe and yields the empty message.
	detail, _ := value.(map[string]any)
	message := util.FirstNonEmpty(ParseLooseStringAny(detail["message"]), ParseLooseStringAny(value))
	if message == "" {
		// Kept separate from FirstNonEmpty, which would trim the upstream text.
		message = StreamString(ev["message"])
	}
	message = util.FirstNonEmptyUntrimmed(message, "upstream response failed")
	return fmt.Errorf("%s", message)
}
