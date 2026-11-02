package grok

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"orchids-api/internal/responses"
	"orchids-api/internal/testutil"
)

var webSearchSchema = map[string]any{
	"type": "object", "additionalProperties": false, "required": []any{"query"},
	"properties": map[string]any{
		"query":           map[string]any{"type": "string"},
		"allowed_domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"limit":           map[string]any{"type": "integer"},
	},
}

func sseEvent(payload map[string]any) string {
	raw, _ := json.Marshal(payload)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", payload["type"], raw)
}

func repairStream(t *testing.T, schemas map[string]map[string]any, events ...string) string {
	t.Helper()
	out, err := io.ReadAll(newToolArgumentsRepairStream(io.NopCloser(strings.NewReader(strings.Join(events, ""))), schemas))
	testutil.NoError(t, err)
	return string(out)
}

type deliveredEvent struct {
	kind    string
	payload map[string]any
}

func readDelivered(t *testing.T, body string) []deliveredEvent {
	t.Helper()
	var events []deliveredEvent
	testutil.NoError(t, responses.ReadSSEBytes(strings.NewReader(body), func(_ string, raw []byte) error {
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			return err
		}
		kind, _ := payload["type"].(string)
		events = append(events, deliveredEvent{kind, payload})
		return nil
	}))
	return events
}

func TestRepairFunctionArgumentsDropsForbiddenPropertiesAndFixesIntegers(t *testing.T) {
	// The live failure: Claude Code's WebSearch forbids extra properties and
	// Build added num_results, so every WebSearch turn used to fail with 502.
	got := repairFunctionArguments("WebSearch", `{"query":"go","num_results":5,"limit":10.0}`, webSearchSchema, false)
	testutil.Equal(t, got, `{"limit":10,"query":"go"}`)
	const valid = `{"query":"go",  "limit":3}`
	testutil.Equal(t, repairFunctionArguments("WebSearch", valid, webSearchSchema, false), valid)
	testutil.Equal(t, repairFunctionArguments("WebSearch", `not json`, webSearchSchema, false), `not json`)
	// Values outside the exactly representable integer range are not rounded.
	testutil.Equal(t, repairFunctionArguments("WebSearch", `{"query":"q","limit":1e300}`, webSearchSchema, false), `{"query":"q","limit":1e300}`)
	// A schema with combinators may declare keys elsewhere; nothing is removed.
	open := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"a": map[string]any{}}, "anyOf": []any{map[string]any{"properties": map[string]any{"b": map[string]any{}}}}}
	testutil.Equal(t, repairFunctionArguments("t", `{"a":1,"b":2}`, open, false), `{"a":1,"b":2}`)
	// Nested objects resolved through $defs are repaired too.
	nested := map[string]any{"type": "object", "$defs": map[string]any{"opt": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"n": map[string]any{"type": "integer"}}}}, "properties": map[string]any{"o": map[string]any{"$ref": "#/$defs/opt"}}}
	testutil.Equal(t, repairFunctionArguments("t", `{"o":{"n":2.0,"x":1}}`, nested, false), `{"o":{"n":2}}`)
}

func TestToolArgumentsRepairStreamDeliversTextImmediatelyAndRepairsCalls(t *testing.T) {
	schemas := map[string]map[string]any{"WebSearch": webSearchSchema}
	text := sseEvent(map[string]any{"type": "response.output_text.delta", "delta": "Searching"})
	body := repairStream(t, schemas,
		text,
		sseEvent(map[string]any{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": "fc1", "name": "WebSearch", "arguments": ""}}),
		sseEvent(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc1", "delta": `{"query":"go",`}),
		sseEvent(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc1", "delta": `"num_results":5}`}),
		sseEvent(map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc1", "arguments": `{"query":"go","num_results":5}`}),
		sseEvent(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "function_call", "id": "fc1", "name": "WebSearch", "arguments": `{"query":"go","num_results":5}`}}),
		sseEvent(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{map[string]any{"type": "function_call", "id": "fc1", "name": "WebSearch", "arguments": `{"query":"go","num_results":5}`}}}}),
	)
	testutil.True(t, strings.HasPrefix(body, text), "text frame must be forwarded first and byte for byte")
	const want = `{"query":"go"}`
	deltas := 0
	for _, event := range readDelivered(t, body) {
		switch event.kind {
		case "response.function_call_arguments.delta":
			deltas++
			testutil.Equal(t, event.payload["delta"], want)
		case "response.function_call_arguments.done":
			testutil.Equal(t, event.payload["arguments"], want)
		case "response.output_item.done":
			testutil.Equal(t, event.payload["item"].(map[string]any)["arguments"], want)
		case "response.completed":
			output := responses.InterfaceMaps(event.payload["response"].(map[string]any)["output"])
			testutil.Equal(t, output[0]["arguments"], want)
		}
	}
	testutil.Equal(t, deltas, 1)
}

func TestToolArgumentsRepairStreamReleasesWithoutArgumentsDone(t *testing.T) {
	schemas := map[string]map[string]any{"WebSearch": webSearchSchema}
	added := sseEvent(map[string]any{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": "fc1", "name": "WebSearch"}})
	delta := sseEvent(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc1", "delta": `{"query":"go","limit":2.0}`})
	for name, ending := range map[string]string{
		"item done": sseEvent(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "function_call", "id": "fc1", "name": "WebSearch"}}),
		"completed": sseEvent(map[string]any{"type": "response.completed", "response": map[string]any{}}),
		"eof":       "",
	} {
		t.Run(name, func(t *testing.T) {
			var got string
			for _, event := range readDelivered(t, repairStream(t, schemas, added, delta, ending)) {
				if event.kind == "response.function_call_arguments.delta" {
					got += event.payload["delta"].(string)
				}
			}
			testutil.Equal(t, got, `{"limit":2,"query":"go"}`)
		})
	}
}

func TestToolArgumentsRepairStreamPassesOversizedArgumentsThrough(t *testing.T) {
	schemas := map[string]map[string]any{"WebSearch": webSearchSchema}
	big := strings.Repeat("x", maxHeldFunctionArgumentsBytes)
	var got string
	for _, event := range readDelivered(t, repairStream(t, schemas,
		sseEvent(map[string]any{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": "fc1", "name": "WebSearch"}}),
		sseEvent(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc1", "delta": `{"query":"`}),
		sseEvent(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc1", "delta": big}),
		sseEvent(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc1", "delta": `"}`}),
	)) {
		if event.kind == "response.function_call_arguments.delta" {
			got += event.payload["delta"].(string)
		}
	}
	testutil.Equal(t, got, `{"query":"`+big+`"}`)
}

func TestToolArgumentsRepairLeavesUndeclaredToolsAndOtherFramesUntouched(t *testing.T) {
	frames := []string{
		": keepalive\n\n",
		sseEvent(map[string]any{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": "fc9", "name": "Other"}}),
		sseEvent(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc9", "delta": `{"n":1.0}`}),
		sseEvent(map[string]any{"type": "response.reasoning_summary_text.delta", "delta": "thinking"}),
	}
	body := repairStream(t, map[string]map[string]any{"WebSearch": webSearchSchema}, frames...)
	testutil.Equal(t, body, strings.Join(frames, ""))
}

func TestRepairResponseJSONRepairsOutputItems(t *testing.T) {
	raw := []byte(`{"output":[{"type":"function_call","name":"WebSearch","arguments":"{\"query\":\"go\",\"num_results\":3}"},{"type":"message","content":[]}]}`)
	var response map[string]any
	testutil.NoError(t, json.Unmarshal(repairResponseJSON(raw, map[string]map[string]any{"WebSearch": webSearchSchema}), &response))
	testutil.Equal(t, responses.InterfaceMaps(response["output"])[0]["arguments"], `{"query":"go"}`)
	untouched := []byte(`{"output":[{"type":"message"}]}`)
	testutil.Equal(t, string(repairResponseJSON(untouched, map[string]map[string]any{"WebSearch": webSearchSchema})), string(untouched))
}

// BenchmarkToolArgumentsRepairStreamText measures the cost the repairer adds to
// an ordinary text stream: frames without a function call are forwarded
// without JSON decoding.
func BenchmarkToolArgumentsRepairStreamText(b *testing.B) {
	var stream strings.Builder
	for i := range 2000 {
		stream.WriteString(sseEvent(map[string]any{"type": "response.output_text.delta", "delta": fmt.Sprintf("token %d ", i)}))
	}
	body := stream.String()
	schemas := map[string]map[string]any{"WebSearch": webSearchSchema}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = io.Copy(io.Discard, newToolArgumentsRepairStream(io.NopCloser(strings.NewReader(body)), schemas))
	}
}
