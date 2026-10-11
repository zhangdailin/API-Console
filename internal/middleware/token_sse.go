package middleware

import (
	"bytes"
	"encoding/json"
	"strings"
)

func (w *TracedResponseWriter) isSSE() bool {
	return strings.HasPrefix(strings.ToLower(w.Header().Get("Content-Type")), "text/event-stream")
}

// tokenSSEDetector parses complete events across arbitrary Write boundaries.
// Only generated text, reasoning or tool content contributes a TTFT sample.
// Oversized events are skipped with bounded memory, without changing the stream.
type tokenSSEDetector struct {
	visible, reasoning, tool bool
	finish                   string
	line                     []byte
	data                     []byte
	event                    string
	size                     int
	overflow                 bool
	lineNonempty             bool
}

const maxTokenEventBytes = 1 << 20

func (d *tokenSSEDetector) observe(chunk []byte) bool {
	anyGenerated := false
	for _, b := range chunk {
		if b != '\n' {
			if b != '\r' {
				d.lineNonempty = true
			}
			d.size++
			if d.size > maxTokenEventBytes {
				d.overflow = true
			}
			if !d.overflow {
				d.line = append(d.line, b)
			}
			continue
		}
		line := bytes.TrimSuffix(d.line, []byte{'\r'})
		if !d.lineNonempty {
			generated := false
			if !d.overflow {
				s := streamOutputSignals(d.event, d.data)
				generated = s.visible || s.reasoning || s.tool
				d.visible = d.visible || s.visible
				d.reasoning = d.reasoning || s.reasoning
				d.tool = d.tool || s.tool
				if s.finish != "" {
					d.finish = s.finish
				}
			}
			d.event, d.size, d.overflow = "", 0, false
			d.line = reusableTokenBuffer(d.line)
			d.data = reusableTokenBuffer(d.data)
			anyGenerated = anyGenerated || generated
			continue
		}
		if !d.overflow {
			field, value, _ := bytes.Cut(line, []byte{':'})
			value = bytes.TrimPrefix(value, []byte{' '})
			switch string(field) {
			case "event":
				d.event = string(value)
			case "data":
				if len(d.data) > 0 {
					d.data = append(d.data, '\n')
				}
				d.data = append(d.data, value...)
			}
		}
		d.line = d.line[:0]
		d.lineNonempty = false
	}
	return anyGenerated
}

// Do not keep a one-off large model event alive for the rest of the request.
func reusableTokenBuffer(buffer []byte) []byte {
	if cap(buffer) > 64<<10 {
		return nil
	}
	return buffer[:0]
}

func nonemptyJSONString(raw json.RawMessage) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil && s != ""
}
