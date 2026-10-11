package cline

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"orchids-api/internal/util"
)

// MiMo's upstream tool decoder can stop at {"chars": when ETX appears in a
// write_stdin call while also emitting the full call as content. Only recover that
// observed shape with an existing native identity; ordinary text is never a
// source of new calls. The shared handler still checks the declared tool name.
type controlToolRecovery struct {
	call      *util.ToolCall
	text      strings.Builder
	ambiguous bool
	overflow  bool
	resolved  bool
	disabled  bool
}

func (r *controlToolRecovery) observe(call *util.ToolCall) {
	if r.resolved || call.ID == "" || call.Name != "write_stdin" || strings.TrimSpace(call.Arguments) != `{"chars":` {
		return
	}
	if r.call != nil && r.call != call {
		r.ambiguous = true
		return
	}
	r.call = call
}

func (r *controlToolRecovery) capture(text string) (string, bool) {
	if text == "" || r.resolved || r.disabled {
		return text, false
	}
	const prolog = "<tool_call><function=write_stdin><parameter=chars>\x03</parameter>"
	combined := r.text.String() + text
	if !strings.HasPrefix(prolog, combined) && !strings.HasPrefix(combined, prolog) {
		r.text.Reset()
		r.disabled = true
		return combined, false
	}
	if len(combined) > 16*1024 {
		r.overflow = true
		r.disabled = true
		r.text.Reset()
		if r.call == nil {
			return combined, false // Preserve oversized ordinary text.
		}
		return "", true
	}
	r.text.WriteString(text)
	return "", true
}

func (r *controlToolRecovery) resolve() (string, error) {
	if r.resolved {
		return "", nil
	}
	r.resolved = true
	if r.call == nil || (r.text.Len() == 0 && !r.overflow) || strings.TrimSpace(r.call.Arguments) != `{"chars":` {
		return r.text.String(), nil // No matching broken native call.
	}
	if r.ambiguous || r.overflow {
		return "", fmt.Errorf("cline stream protocol error: ambiguous control-character tool call")
	}
	args, ok := parseControlToolFallback(r.text.String())
	if !ok {
		return "", fmt.Errorf("cline stream protocol error: incomplete control-character tool call")
	}
	r.call.Arguments = args
	return "", nil
}

func parseControlToolFallback(text string) (string, bool) {
	const prefix = "<tool_call><function=write_stdin>"
	const suffix = "</function></tool_call>"
	if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, suffix) {
		return "", false
	}
	text = text[len(prefix) : len(text)-len(suffix)]
	values := map[string]interface{}{}
	for text != "" {
		if !strings.HasPrefix(text, "<parameter=") {
			return "", false
		}
		text = text[len("<parameter="):]
		name, rest, ok := strings.Cut(text, ">")
		if !ok {
			return "", false
		}
		value, rest, ok := strings.Cut(rest, "</parameter>")
		if !ok {
			return "", false
		}
		if _, duplicate := values[name]; duplicate {
			return "", false
		}
		switch name {
		case "chars":
			if value != "\x03" {
				return "", false
			}
			values[name] = value
		case "session_id", "max_output_tokens", "yield_time_ms":
			v, err := strconv.ParseInt(value, 10, 64)
			if err != nil || v < 0 || (name == "session_id" && v == 0) {
				return "", false
			}
			values[name] = v
		default:
			return "", false
		}
		text = rest
	}
	if len(values) != 4 {
		return "", false
	}
	encoded, err := json.Marshal(values)
	return string(encoded), err == nil
}
