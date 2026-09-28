package grok

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/util"
)

var (
	grokJSONEmptyObjectBytes = []byte("{}")
	allowedMessageRoles      = map[string]struct{}{
		"developer": {},
		"system":    {},
		"user":      {},
		"assistant": {},
		"tool":      {},
	}
	userContentTypes = map[string]struct{}{
		"text":        {},
		"image_url":   {},
		"input_audio": {},
		"file":        {},
		// The Anthropic Messages front end lowers its blocks onto this same
		// validator, so the Responses-shaped parts it produces are valid here
		// too. Without them a document or a multi-part tool_result is rejected
		// before the request ever reaches the upstream.
		"input_text":  {},
		"input_image": {},
		"input_file":  {},
	}
)

func randomHex(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

func randomUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4],
		buf[4:6],
		buf[6:8],
		buf[8:10],
		buf[10:16],
	)
}

func parseUpstreamLines(body io.Reader, onLine func(map[string]interface{}) error) error {
	decoder := json.NewDecoder(body)

	for {
		var line map[string]interface{}
		if err := decoder.Decode(&line); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if message := upstreamStreamErrorMessage(line); message != "" {
			return fmt.Errorf("grok upstream stream error: %s", message)
		}
		result, _ := line["result"].(map[string]interface{})
		if message := upstreamStreamErrorMessage(result); message != "" {
			return fmt.Errorf("grok upstream stream error: %s", message)
		}
		resp, _ := result["response"].(map[string]interface{})
		if resp == nil {
			continue
		}
		if err := onLine(resp); err != nil {
			return err
		}
	}
}

func upstreamStreamErrorMessage(value map[string]interface{}) string {
	if value == nil {
		return ""
	}
	if raw, exists := value["error"]; exists {
		if message := upstreamErrorValueMessage(raw); message != "" {
			return message
		}
		return "upstream returned an unspecified error"
	}
	// Current Grok streams can also carry failures as an event envelope rather
	// than a top-level error.  Do not silently turn that into a successful empty
	// completion just because app-chat normally uses result.response envelopes.
	event, _ := value["event"].(map[string]interface{})
	if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(event["type"])), "error") {
		return ""
	}
	if raw, exists := event["error"]; exists {
		if message := upstreamErrorValueMessage(raw); message != "" {
			return message
		}
		return "upstream returned an unspecified error"
	}
	if message := upstreamErrorValueMessage(event); message != "" {
		return message
	}
	return "upstream returned an unspecified error"
}

func upstreamErrorValueMessage(raw interface{}) string {
	if raw == nil {
		return ""
	}
	message := strings.TrimSpace(fmt.Sprint(raw))
	if details, ok := raw.(map[string]interface{}); ok {
		message = firstNonEmpty(
			strings.TrimSpace(fmt.Sprint(details["message"])),
			strings.TrimSpace(fmt.Sprint(details["error"])),
			strings.TrimSpace(fmt.Sprint(details["code"])),
		)
	}
	if message == "" || message == "<nil>" {
		return ""
	}
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

// firstNonEmpty delegates to the shared implementation in internal/util so the
// package keeps its short local name without duplicating the logic.
func firstNonEmpty(values ...string) string {
	return util.FirstNonEmpty(values...)
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func encodeJSONBytes(v interface{}) []byte {
	buf := bytes.Buffer{}
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return grokJSONEmptyObjectBytes
	}
	raw := buf.Bytes()
	if n := len(raw); n > 0 && raw[n-1] == '\n' {
		return raw[:n-1]
	}
	return raw
}

func parseDataURI(input string) (fileName, contentBase64, mime string, err error) {
	s := strings.TrimSpace(input)
	if !strings.HasPrefix(strings.ToLower(s), "data:") {
		return "", "", "", errors.New("not a data uri")
	}
	idx := strings.Index(s, ",")
	if idx <= 0 {
		return "", "", "", errors.New("invalid data uri")
	}
	header := s[5:idx]
	payload := strings.TrimSpace(s[idx+1:])
	if !strings.Contains(strings.ToLower(header), ";base64") {
		return "", "", "", errors.New("data uri is not base64 encoded")
	}
	mime = strings.TrimSpace(strings.Split(header, ";")[0])
	if mime == "" {
		mime = "application/octet-stream"
	}
	ext := "bin"
	if slash := strings.Index(mime, "/"); slash >= 0 && slash+1 < len(mime) {
		ext = strings.TrimSpace(mime[slash+1:])
	}
	return "file." + ext, payload, mime, nil
}

func uniqueStrings(input []string) []string {
	return util.UniqueStrings(input)
}

func interfaceToInt(v interface{}) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return i
		}
	}
	return 0
}

func interfaceSlice(v interface{}) []interface{} {
	switch x := v.(type) {
	case []interface{}:
		return x
	default:
		return nil
	}
}
