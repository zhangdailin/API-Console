package util

import (
	"bytes"
	"encoding/json"
)

var jsonEmptyObjectBytes = []byte("{}")

// EncodeJSONBytes renders v as compact JSON with HTML escaping turned off.
//
// Escaping is off because the payload goes into an SSE data line and into
// upstream bodies: turning <, > and & into \u003c-style escapes changes bytes
// without changing meaning, and the upstream compares them.
func EncodeJSONBytes(v any) []byte {
	buf := bytes.Buffer{}
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return jsonEmptyObjectBytes
	}
	raw := buf.Bytes()
	if n := len(raw); n > 0 && raw[n-1] == '\n' {
		return raw[:n-1]
	}
	return raw
}
