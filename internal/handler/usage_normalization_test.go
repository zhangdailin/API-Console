package handler

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"orchids-api/internal/adapter"
)

func exclusiveUsageFixture() map[string]any {
	return map[string]any{
		"inputTokensSemantics": "exclusive_cache", "inputTokens": 130,
		"cacheReadTokens": 26892, "cacheWriteTokens": 64, "outputTokens": 506,
	}
}

func TestExclusiveUsageCanonicalAndMessagesProjection(t *testing.T) {
	sh, _ := newStreamTestHandler(t, false, adapter.FormatAnthropic)
	for range 3 {
		sh.applyUpstreamUsageTokens(exclusiveUsageFixture())
	}
	if sh.inputTokens != 27086 || sh.cachedInputTokens != 26892 || sh.cacheWriteTokens != 64 || sh.outputTokens != 506 || sh.totalUsageTokens() != 27592 || !sh.hasAuthoritativeUsage() {
		t.Fatalf("bad canonical usage or cumulative double-counting: input=%d cached=%d write=%d output=%d total=%d", sh.inputTokens, sh.cachedInputTokens, sh.cacheWriteTokens, sh.outputTokens, sh.totalUsageTokens())
	}
	messages := sh.messagesUsage()
	if messages["input_tokens"] != 130 || messages["cache_read_input_tokens"] != 26892 || messages["cache_creation_input_tokens"] != 64 || messages["output_tokens"] != 506 {
		t.Fatalf("Messages usage was not exclusive: %v", messages)
	}
	chat := sh.exclusiveWireUsage(true)
	if chat["input_tokens"] != 27086 || chat["cache_read_input_tokens"] != 26892 {
		t.Fatalf("Chat usage was not inclusive: %v", chat)
	}
}

func TestExclusiveUsageMissingFieldsRemainUnknown(t *testing.T) {
	sh, _ := newStreamTestHandler(t, false, adapter.FormatAnthropic)
	sh.inputTokens = 777
	sh.applyUpstreamUsageTokens(map[string]any{"inputTokensSemantics": "exclusive_cache", "inputTokens": 130, "cacheReadTokens": 26892, "cacheWriteTokens": nil, "outputTokens": nil})
	if sh.inputTokens != 777 || sh.hasAuthoritativeUsage() {
		t.Fatalf("partial observation replaced estimate or became billable: %d", sh.inputTokens)
	}
	for _, field := range []string{"cache_creation_input_tokens", "output_tokens"} {
		if _, exists := sh.messagesUsage()[field]; exists {
			t.Fatalf("invented Messages %s", field)
		}
	}
	if _, exists := sh.exclusiveWireUsage(true)["input_tokens"]; exists {
		t.Fatal("incomplete components claimed a total input")
	}
	sh.applyUpstreamUsageTokens(map[string]any{"inputTokensSemantics": "exclusive_cache", "cacheWriteTokens": 0, "outputTokens": 0})
	if sh.inputTokens != 27022 || !sh.hasAuthoritativeUsage() {
		t.Fatalf("explicit zero was not observed: %d", sh.inputTokens)
	}
}

func TestExclusiveUsageOnlyExactMarkerOptsIn(t *testing.T) {
	for _, marker := range []any{nil, "inclusive", "EXCLUSIVE_CACHE", true} {
		sh, _ := newStreamTestHandler(t, false, adapter.FormatAnthropic)
		usage := exclusiveUsageFixture()
		usage["inputTokensSemantics"] = marker
		sh.applyUpstreamUsageTokens(usage)
		if sh.inputTokens != 130 || sh.exclusiveUsage.active || sh.messagesUsage()["input_tokens"] != 130 {
			t.Fatalf("unmarked provider changed semantics for %v: %d", marker, sh.inputTokens)
		}
	}
}

func TestExclusiveUsageOverflowAndMalformedFailClosed(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for name, value := range map[string]any{
		"negative": -1, "fraction": 1.5, "nan": math.NaN(), "inf": math.Inf(1),
		"uint overflow": ^uint64(0), "json overflow": json.Number("18446744073709551616"),
		"sum overflow": maxInt,
	} {
		t.Run(name, func(t *testing.T) {
			sh, _ := newStreamTestHandler(t, false, adapter.FormatAnthropic)
			sh.inputTokens = 777
			usage := exclusiveUsageFixture()
			usage["inputTokens"] = value
			sh.applyUpstreamUsageTokens(usage)
			if sh.inputTokens != 777 || sh.inputTokens < 0 || sh.outputTokens < 0 || sh.hasAuthoritativeUsage() {
				t.Fatalf("invalid count became canonical/billable: input=%d output=%d", sh.inputTokens, sh.outputTokens)
			}
		})
	}
}

func TestExclusiveUsageTotalOverflowFailsClosed(t *testing.T) {
	sh, _ := newStreamTestHandler(t, false, adapter.FormatAnthropic)
	sh.inputTokens = 777
	sh.applyUpstreamUsageTokens(map[string]any{
		"inputTokensSemantics": "exclusive_cache", "inputTokens": int(^uint(0) >> 1),
		"cacheReadTokens": 0, "cacheWriteTokens": 0, "outputTokens": 506,
	})
	if sh.hasAuthoritativeUsage() || sh.inputTokens != 777 || sh.totalUsageTokens() < 0 {
		t.Fatalf("total overflow became canonical usage: input=%d output=%d total=%d", sh.inputTokens, sh.outputTokens, sh.totalUsageTokens())
	}
}

func TestExclusiveUsageMessagesOpeningAndTerminal(t *testing.T) {
	sh, rec := newTerminalUsageTestHandler(t)
	sh.applyUpstreamUsageTokens(exclusiveUsageFixture())
	sh.finishResponse("end_turn")
	usage := terminalMessagesUsage(t, rec.Body.String())
	if usage["input_tokens"] != 130 || usage["cache_read_input_tokens"] != 26892 || usage["cache_creation_input_tokens"] != 64 || usage["output_tokens"] != 506 {
		t.Fatalf("terminal projection mismatch: %v", usage)
	}
	var opening struct {
		Message struct {
			Usage map[string]int `json:"usage"`
		} `json:"message"`
	}
	frames := splitSSEDataForUsageTest(rec.Body.String())
	if err := json.Unmarshal([]byte(frames[0]), &opening); err != nil {
		t.Fatal(err)
	}
	if opening.Message.Usage["input_tokens"] != 130 || opening.Message.Usage["cache_read_input_tokens"] != 26892 {
		t.Fatalf("opening projection mismatch: %v", opening.Message.Usage)
	}
}

func splitSSEDataForUsageTest(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			out = append(out, data)
		}
	}
	return out
}

func TestExclusiveUsageChatFramesDoNotInventMissingTotals(t *testing.T) {
	for _, complete := range []bool{false, true} {
		sh, rec := newStreamTestHandler(t, true, adapter.FormatOpenAI)
		usage := exclusiveUsageFixture()
		if !complete {
			delete(usage, "cacheWriteTokens")
		}
		sh.applyUpstreamUsageTokens(usage)
		sh.finishResponse("end_turn")
		var terminal struct {
			Usage map[string]any `json:"usage"`
		}
		for _, data := range splitSSEDataForUsageTest(rec.buf.String()) {
			if data == "[DONE]" {
				continue
			}
			var frame struct {
				Usage map[string]any `json:"usage"`
			}
			if err := json.Unmarshal([]byte(data), &frame); err != nil {
				t.Fatal(err)
			}
			if frame.Usage != nil {
				terminal.Usage = frame.Usage
			}
		}
		if terminal.Usage == nil || terminal.Usage["completion_tokens"] != float64(506) {
			t.Fatalf("Chat terminal output missing: %v", terminal.Usage)
		}
		if complete {
			if terminal.Usage["prompt_tokens"] != float64(27086) || terminal.Usage["total_tokens"] != float64(27592) {
				t.Fatalf("Chat canonical counters changed: %v", terminal.Usage)
			}
		} else {
			for _, key := range []string{"prompt_tokens", "total_tokens", "cache_creation_input_tokens"} {
				if _, exists := terminal.Usage[key]; exists {
					t.Fatalf("partial Chat usage invented %s: %v", key, terminal.Usage)
				}
			}
		}
	}
}

func TestExclusiveUsageResetClearsContract(t *testing.T) {
	sh, _ := newStreamTestHandler(t, false, adapter.FormatAnthropic)
	sh.applyUpstreamUsageTokens(exclusiveUsageFixture())
	sh.resetRoundState()
	sh.applyUpstreamUsageTokens(map[string]any{"inputTokens": 19, "outputTokens": 2, "cacheReadTokens": 10})
	if sh.exclusiveUsage.active || sh.inputTokens != 19 || sh.messagesUsage()["input_tokens"] != 19 {
		t.Fatal("exclusive contract survived round reset")
	}
}

func TestLegacyInclusiveCachedUsageIsNotAddedTwice(t *testing.T) {
	for _, format := range []adapter.ResponseFormat{adapter.FormatOpenAI, adapter.FormatAnthropic} {
		sh, rec := newStreamTestHandler(t, true, format)
		sh.canonicalWireUsage = true
		sh.applyUpstreamUsageTokens(map[string]any{"inputTokens": 1000, "cacheReadTokens": 900, "outputTokens": 20})
		sh.finishResponse("end_turn")
		var terminal map[string]any
		for _, data := range splitSSEDataForUsageTest(rec.buf.String()) {
			var frame map[string]any
			if json.Unmarshal([]byte(data), &frame) == nil {
				if u, ok := frame["usage"].(map[string]any); ok {
					terminal = u
				}
			}
		}
		if terminal["prompt_tokens"] != float64(1000) {
			t.Fatalf("legacy inclusive prompt doublecounted: %v", terminal)
		}
	}
}
func TestIncompleteContributorsCannotSynthesizeAuthoritativeInput(t *testing.T) {
	sh, _ := newStreamTestHandler(t, false, adapter.FormatAnthropic)
	sh.inputTokens = 777
	usage := exclusiveUsageFixture()
	usage["inputTokensIncomplete"] = true
	sh.applyUpstreamUsageTokens(usage)
	if sh.hasAuthoritativeUsage() || sh.inputTokens != 777 {
		t.Fatal("partial contributors became billable")
	}
}
