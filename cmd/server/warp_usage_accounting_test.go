package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	"orchids-api/internal/audit"
	"orchids-api/internal/middleware"
	"orchids-api/internal/pricing"
	"orchids-api/internal/store"
)

// The capture is synchronous at the handler boundary. Waiting for its first
// event also waits for settlement, without racing streaming response completion.
type warpUsageAuditCapture struct {
	mu     sync.Mutex
	events []audit.Event
	done   chan struct{}
}

func (c *warpUsageAuditCapture) Log(_ context.Context, event audit.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
	if len(c.events) == 1 {
		close(c.done)
	}
}

func (c *warpUsageAuditCapture) single(t *testing.T) audit.Event {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not produce an audit event")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) != 1 {
		t.Fatalf("audit events = %d, want exactly one", len(c.events))
	}
	return c.events[0]
}

func warpUsageCharges(count *warpapi.TokenCount) *warpapi.RequestCharges {
	return warpapi.RequestCharges_builder{UsageByCategory: map[string]*warpapi.ChargedUsage{
		"primary_agent": warpapi.ChargedUsage_builder{DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{
			warpRouteModel: warpapi.InferenceUsage_builder{TokenCount: count}.Build(),
		}}.Build(),
	}}.Build()
}

func warpUsageDistinctCharges() *warpapi.RequestCharges {
	return warpUsageCharges(warpapi.TokenCount_builder{
		Input: proto.Uint64(130), InputCacheRead: proto.Uint64(26892),
		InputCacheWrite: proto.Uint64(64), Output: proto.Uint64(506),
	}.Build())
}

// Unlike newWarpRouteStub, this fixture includes request-only usage in the
// official protobuf SSE. All origins still point at one loopback listener.
func newWarpUsageRouteStub(t *testing.T, finish *warpapi.ResponseEvent_StreamFinished, metadata *warpapi.RequestCharges) *warpRouteStub {
	t.Helper()
	messages := []*warpapi.Message{warpapi.Message_builder{
		Id:          proto.String("usage-answer"),
		AgentOutput: warpapi.Message_AgentOutput_builder{Text: proto.String(warpRouteAnswer)}.Build(),
	}.Build()}
	if metadata != nil {
		messages = append(messages, warpapi.Message_builder{RequestMetadata: warpapi.Message_RequestMetadata_builder{
			Charges: metadata, Outcome: warpapi.Message_RequestMetadata_OUTCOME_COMPLETED.Enum(),
		}.Build()}.Build())
	}
	finish.SetDone(warpapi.ResponseEvent_StreamFinished_Done_builder{}.Build())
	var stream strings.Builder
	for _, event := range []*warpapi.ResponseEvent{
		warpapi.ResponseEvent_builder{ClientActions: warpapi.ResponseEvent_ClientActions_builder{
			Actions: []*warpapi.ClientAction{warpapi.ClientAction_builder{AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{Messages: messages}.Build()}.Build()},
		}.Build()}.Build(),
		warpapi.ResponseEvent_builder{Finished: finish}.Build(),
	} {
		raw, err := proto.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&stream, "data: %s\n\n", base64.RawURLEncoding.EncodeToString(raw))
	}
	stub := &warpRouteStub{requests: make(chan []byte, 16)}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ai/multi-agent" {
			stub.otherCalls.Add(1)
			http.NotFound(w, r)
			return
		}
		stub.aiCalls.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-protobuf" || r.Header.Get("Accept") != "text/event-stream" {
			t.Error("Warp usage request must POST protobuf and accept SSE")
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		stub.requests <- raw
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream.String())
	}))
	t.Cleanup(stub.Close)
	return stub
}

func warpUsageDecodeObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		t.Fatalf("invalid JSON object: %v: %s", err, raw)
	}
	return object
}

// Merge Messages snapshots rather than adding them: the terminal delta replaces
// any opening estimate. Chat and Responses must each carry terminal usage too.
func warpUsageFromWire(t *testing.T, protocol string, streaming bool, body string) map[string]any {
	t.Helper()
	if !streaming {
		usage, _ := warpUsageDecodeObject(t, body)["usage"].(map[string]any)
		if usage == nil {
			t.Fatalf("%s JSON omitted usage: %s", protocol, body)
		}
		return usage
	}
	usage := map[string]any{}
	terminal := 0
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		object := warpUsageDecodeObject(t, strings.TrimPrefix(line, "data: "))
		var update map[string]any
		switch protocol {
		case "messages":
			if object["type"] == "message_start" {
				message, _ := object["message"].(map[string]any)
				update, _ = message["usage"].(map[string]any)
			} else if object["type"] == "message_delta" {
				update, _ = object["usage"].(map[string]any)
				terminal++
			}
		case "responses":
			if object["type"] == "response.completed" {
				response, _ := object["response"].(map[string]any)
				update, _ = response["usage"].(map[string]any)
				terminal++
			}
		default:
			update, _ = object["usage"].(map[string]any)
			if update != nil {
				terminal++
			}
		}
		for key, value := range update {
			usage[key] = value
		}
	}
	if terminal != 1 || len(usage) == 0 {
		t.Fatalf("%s SSE terminal usage count = %d, usage = %#v: %s", protocol, terminal, usage, body)
	}
	return usage
}

func warpUsageWantCounter(t *testing.T, usage map[string]any, key string, want int) {
	t.Helper()
	if got, exists := usage[key]; !exists || got != float64(want) {
		t.Errorf("usage.%s = %#v (present=%t), want %d; usage=%#v", key, got, exists, want, usage)
	}
}

func warpUsageAssertCanonicalAudit(t *testing.T, event audit.Event) {
	t.Helper()
	if event.Kind != audit.KindRequest || event.Status != "success" || event.Channel != "warp" || event.Model != warpRouteModel || event.APIKeyID == 0 || event.AccountID == 0 {
		t.Errorf("unexpected request audit identity/status: %+v", event)
	}
	if event.UsageSource != audit.UsageSourceUpstream || event.InputTokens != 27086 || event.CachedInputTokens != 26892 || event.CacheWriteTokens != 64 || event.OutputTokens != 506 || event.TotalTokens != 27592 {
		t.Errorf("audit must retain inclusive, request-only upstream usage: %+v", event)
	}
	if event.PricingModel != "" || event.CostInUSDTicks != 0 {
		t.Errorf("synthetic model must remain unpriced, not receive an invented rate: %+v", event)
	}
}

func TestWarpUsageAccountingProtocolRoutes(t *testing.T) {
	for _, protocol := range []string{"messages", "chat/completions", "responses"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, streaming), func(t *testing.T) {
				stub := newWarpUsageRouteStub(t, warpapi.ResponseEvent_StreamFinished_builder{RequestCharges: warpUsageDistinctCharges()}.Build(), nil)
				e, s, h := newWarpRouteE2E(t, stub, 1)
				capture := &warpUsageAuditCapture{done: make(chan struct{})}
				h.SetAuditLogger(capture)
				request := map[string]any{"model": warpRouteModel, "stream": streaming}
				if protocol == "responses" {
					request["input"] = warpRouteQuery
				} else {
					request["messages"] = []map[string]string{{"role": "user", "content": warpRouteQuery}}
				}
				raw, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				resp := e.do(t, http.MethodPost, "/warp/v1/"+protocol, string(raw), false)
				body := e.readBody(t, resp)
				if resp.StatusCode != http.StatusOK || !strings.Contains(body, warpRouteAnswer) {
					t.Fatalf("route = %d %s", resp.StatusCode, body)
				}
				usage := warpUsageFromWire(t, protocol, streaming, body)
				switch protocol {
				case "messages":
					warpUsageWantCounter(t, usage, "input_tokens", 130)
					warpUsageWantCounter(t, usage, "cache_read_input_tokens", 26892)
					warpUsageWantCounter(t, usage, "cache_creation_input_tokens", 64)
					warpUsageWantCounter(t, usage, "output_tokens", 506)
				case "chat/completions":
					warpUsageWantCounter(t, usage, "prompt_tokens", 27086)
					warpUsageWantCounter(t, usage, "completion_tokens", 506)
					warpUsageWantCounter(t, usage, "total_tokens", 27592)
					details, _ := usage["prompt_tokens_details"].(map[string]any)
					warpUsageWantCounter(t, details, "cached_tokens", 26892)
				case "responses":
					warpUsageWantCounter(t, usage, "input_tokens", 27086)
					warpUsageWantCounter(t, usage, "output_tokens", 506)
					warpUsageWantCounter(t, usage, "total_tokens", 27592)
					details, _ := usage["input_tokens_details"].(map[string]any)
					warpUsageWantCounter(t, details, "cached_tokens", 26892)
				}
				warpUsageAssertCanonicalAudit(t, capture.single(t))
				keys, err := s.ListApiKeys(context.Background())
				if err != nil || len(keys) != 1 || keys[0].BillingUsedUSDTicks != 0 {
					t.Fatalf("unpriced synthetic model changed key spend: keys=%+v err=%v", keys, err)
				}
				if stub.aiCalls.Load() != 1 || stub.otherCalls.Load() != 0 {
					t.Fatalf("expected one generation, no refresh/catalog: ai=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
				}
				select {
				case raw := <-stub.requests:
					assertWarpRouteRequest(t, raw)
				default:
					t.Fatal("usage response bypassed the registered Warp client")
				}
			})
		}
	}
}

func TestWarpUsageAccountingRequestChargesNotDoubleCounted(t *testing.T) {
	charges := warpUsageDistinctCharges()
	finish := warpapi.ResponseEvent_StreamFinished_builder{
		RequestCharges: charges,
		RequestCost:    warpapi.ResponseEvent_StreamFinished_RequestCost_builder{Exact: proto.Float32(999)}.Build(),
		TokenUsage: []*warpapi.ResponseEvent_StreamFinished_TokenUsage{
			warpapi.ResponseEvent_StreamFinished_TokenUsage_builder{
				TotalInput: proto.Uint32(999), Output: proto.Uint32(999), InputCacheRead: proto.Uint32(999), InputCacheWrite: proto.Uint32(999),
			}.Build(),
		},
		ConversationUsageMetadata: warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata_builder{
			TotalInputTokens: proto.Uint32(999999), TotalCharges: charges,
		}.Build(),
	}.Build()
	// RequestMetadata repeats the same measurement later carried by Finished.
	stub := newWarpUsageRouteStub(t, finish, charges)
	e, _, h := newWarpRouteE2E(t, stub, 1)
	capture := &warpUsageAuditCapture{done: make(chan struct{})}
	h.SetAuditLogger(capture)
	resp := e.do(t, http.MethodPost, "/warp/v1/chat/completions", `{"model":"`+warpRouteModel+`","messages":[{"role":"user","content":"`+warpRouteQuery+`"}]}`, false)
	body := e.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("route = %d %s", resp.StatusCode, body)
	}
	usage := warpUsageFromWire(t, "chat/completions", false, body)
	warpUsageWantCounter(t, usage, "prompt_tokens", 27086)
	warpUsageWantCounter(t, usage, "total_tokens", 27592)
	warpUsageAssertCanonicalAudit(t, capture.single(t))
	if stub.aiCalls.Load() != 1 || stub.otherCalls.Load() != 0 {
		t.Fatalf("measurement envelopes caused another request: ai=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
	}
}

func TestWarpUsageAccountingInvalidMeasurementFailsClosed(t *testing.T) {
	overflow := warpUsageCharges(warpapi.TokenCount_builder{Input: proto.Uint64(math.MaxUint64)}.Build())
	overflow.GetUsageByCategory()["primary_agent"].GetDirectApiInferenceUsage()["second"] = warpapi.InferenceUsage_builder{
		TokenCount: warpapi.TokenCount_builder{Input: proto.Uint64(1)}.Build(),
	}.Build()
	cases := map[string]*warpapi.RequestCharges{
		"aggregate_uint64_overflow": overflow,
		"individual_int_overflow": warpUsageCharges(warpapi.TokenCount_builder{
			Input: proto.Uint64(uint64(math.MaxInt) + 1), Output: proto.Uint64(1),
		}.Build()),
		"total_tokens_int_overflow": warpUsageCharges(warpapi.TokenCount_builder{
			Input: proto.Uint64(uint64(math.MaxInt)), InputCacheRead: proto.Uint64(0), InputCacheWrite: proto.Uint64(0), Output: proto.Uint64(1),
		}.Build()),
		"inclusive_input_int_overflow": warpUsageCharges(warpapi.TokenCount_builder{
			Input: proto.Uint64(uint64(math.MaxInt)), InputCacheRead: proto.Uint64(1), Output: proto.Uint64(1),
		}.Build()),
		"invalid_negative_cost": warpapi.RequestCharges_builder{UsageByCategory: map[string]*warpapi.ChargedUsage{
			"primary_agent": warpapi.ChargedUsage_builder{PlatformUsageInCredits: proto.Float32(-1)}.Build(),
		}}.Build(),
	}
	for name, charges := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newWarpUsageRouteStub(t, warpapi.ResponseEvent_StreamFinished_builder{RequestCharges: charges}.Build(), nil)
			e, _, h := newWarpRouteE2E(t, stub, 1)
			capture := &warpUsageAuditCapture{done: make(chan struct{})}
			h.SetAuditLogger(capture)
			resp := e.do(t, http.MethodPost, "/warp/v1/chat/completions", `{"model":"`+warpRouteModel+`","messages":[{"role":"user","content":"`+warpRouteQuery+`"}]}`, false)
			body := e.readBody(t, resp)
			if resp.StatusCode != http.StatusBadGateway || strings.Contains(body, warpRouteAnswer) {
				t.Errorf("invalid measured usage must fail without successful content: status=%d body=%s", resp.StatusCode, body)
			}
			event := capture.single(t)
			if event.Status != "error" || event.UsageSource == audit.UsageSourceUpstream || event.CostInUSDTicks != 0 || event.PricingModel != "" {
				t.Errorf("invalid measurement was trusted/priced: %+v", event)
			}
			if stub.aiCalls.Load() != 1 || stub.otherCalls.Load() != 0 {
				t.Fatalf("invalid usage retried generation: ai=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
			}
		})
	}
}

// Count the actual ledger call, not merely the final balance: the store itself
// is idempotent and could otherwise hide duplicate settlement attempts.
type warpUsageCountingLedger struct {
	*store.Store
	settlements atomic.Int64
}

func (l *warpUsageCountingLedger) SettleApiKeyBilling(ctx context.Context, id int64, eventID string, amount int64) error {
	l.settlements.Add(1)
	return l.Store.SettleApiKeyBilling(ctx, id, eventID, amount)
}

func TestWarpUsageAccountingKnownPriceSettlesExactlyOnce(t *testing.T) {
	const model = "grok-4.6"
	want, priced := pricing.EstimateCost(model, 27086, 26892, 506, 27086)
	if !priced || want.CostInUSDTicks <= 0 {
		t.Fatal("fixture must select an exact officially priced model")
	}
	for _, protocol := range []string{"messages", "chat/completions", "responses"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, streaming), func(t *testing.T) {
				charges := warpUsageDistinctCharges()
				stub := newWarpUsageRouteStub(t, warpapi.ResponseEvent_StreamFinished_builder{RequestCharges: charges}.Build(), charges)
				e, s, h := newWarpRouteE2E(t, stub, 1)
				accounts, err := s.ListAccounts(context.Background())
				if err != nil || len(accounts) != 1 {
					t.Fatalf("planted accounts: count=%d err=%v", len(accounts), err)
				}
				accounts[0].WarpModelIDs = []string{model}
				if err := s.UpdateAccount(context.Background(), accounts[0]); err != nil {
					t.Fatal(err)
				}
				if _, err := s.ReconcileDiscoveredModels(context.Background(), "warp", []*store.Model{{
					Channel: "warp", ModelID: model, Name: model, UpstreamModel: model,
					Provider: "warp", Status: store.ModelStatusAvailable, Verified: true,
					Capabilities: []string{store.CapabilityChat, "text"},
				}}, store.ModelReconcileOptions{Prune: true}); err != nil {
					t.Fatal(err)
				}
				keys, err := s.ListApiKeys(context.Background())
				if err != nil || len(keys) != 1 {
					t.Fatalf("planted API keys: count=%d err=%v", len(keys), err)
				}
				key := keys[0]
				key.BillingLimitUSDTicks = 100 * pricing.TicksPerUSD
				if err := s.UpdateApiKey(context.Background(), key); err != nil {
					t.Fatal(err)
				}
				ledger := &warpUsageCountingLedger{Store: s}
				middleware.SetAPIKeyBillingStore(ledger)
				defer middleware.SetAPIKeyBillingStore(nil)
				capture := &warpUsageAuditCapture{done: make(chan struct{})}
				h.SetAuditLogger(capture)
				request := map[string]any{"model": model, "stream": streaming}
				if protocol == "responses" {
					request["input"] = warpRouteQuery
				} else {
					request["messages"] = []map[string]string{{"role": "user", "content": warpRouteQuery}}
				}
				raw, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				resp := e.do(t, http.MethodPost, "/warp/v1/"+protocol, string(raw), false)
				body := e.readBody(t, resp)
				if resp.StatusCode != http.StatusOK || !strings.Contains(body, warpRouteAnswer) {
					t.Fatalf("priced route = %d %s", resp.StatusCode, body)
				}
				event := capture.single(t)
				if event.InputTokens != 27086 || event.CachedInputTokens != 26892 || event.CacheWriteTokens != 64 || event.OutputTokens != 506 || event.TotalTokens != 27592 || event.UsageSource != audit.UsageSourceUpstream {
					t.Errorf("priced request lost canonical accounting: %+v", event)
				}
				if event.PricingModel != want.Model || event.PricingVersion != pricing.Version || event.CostInUSDTicks != want.CostInUSDTicks {
					t.Errorf("audit price = %+v, want %+v", event, want)
				}
				stored, err := s.GetApiKeyByID(context.Background(), key.ID)
				if err != nil || stored.BillingUsedUSDTicks != want.CostInUSDTicks || ledger.settlements.Load() != 1 {
					t.Fatalf("key must settle exactly once: key=%+v calls=%d want=%d err=%v", stored, ledger.settlements.Load(), want.CostInUSDTicks, err)
				}
				if stub.aiCalls.Load() != 1 || stub.otherCalls.Load() != 0 {
					t.Fatalf("priced accounting made extra upstream requests: ai=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
				}
			})
		}
	}
}

func TestWarpUsageAccountingPartialMeasurementDoesNotInventInput(t *testing.T) {
	stub := newWarpUsageRouteStub(t, warpapi.ResponseEvent_StreamFinished_builder{RequestCharges: warpUsageCharges(
		warpapi.TokenCount_builder{Output: proto.Uint64(506)}.Build(),
	)}.Build(), nil)
	e, _, h := newWarpRouteE2E(t, stub, 1)
	capture := &warpUsageAuditCapture{done: make(chan struct{})}
	h.SetAuditLogger(capture)
	resp := e.do(t, http.MethodPost, "/warp/v1/chat/completions", `{"model":"`+warpRouteModel+`","messages":[{"role":"user","content":"`+warpRouteQuery+`"}]}`, false)
	body := e.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("partial measured usage = %d %s", resp.StatusCode, body)
	}
	usage := warpUsageFromWire(t, "chat/completions", false, body)
	warpUsageWantCounter(t, usage, "completion_tokens", 506)
	for _, key := range []string{"prompt_tokens", "total_tokens", "prompt_tokens_details"} {
		if _, exists := usage[key]; exists {
			t.Errorf("partial upstream measurement invented %s: %#v", key, usage)
		}
	}
	event := capture.single(t)
	// A partial row must not promote the local prompt estimate to an entirely
	// upstream-measured, billable request. The known output remains observable.
	if event.OutputTokens != 506 || event.UsageSource == audit.UsageSourceUpstream || event.PricingModel != "" || event.CostInUSDTicks != 0 {
		t.Errorf("partial measurement lost output or promoted estimated input to measured spend: %+v", event)
	}
	if stub.aiCalls.Load() != 1 || stub.otherCalls.Load() != 0 {
		t.Fatalf("partial usage generated extra calls: ai=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
	}
}

func TestWarpUsageAccountingObservedZeroWriteAndPartialContributors(t *testing.T) {
	for _, partial := range []bool{false, true} {
		count := warpapi.TokenCount_builder{Input: proto.Uint64(130), InputCacheRead: proto.Uint64(26892), InputCacheWrite: proto.Uint64(0), Output: proto.Uint64(506)}.Build()
		charges := warpUsageCharges(count)
		if partial {
			charges.GetUsageByCategory()["other"] = warpapi.ChargedUsage_builder{DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{"other": warpapi.InferenceUsage_builder{TokenCount: warpapi.TokenCount_builder{Output: proto.Uint64(1)}.Build()}.Build()}}.Build()
		}
		stub := newWarpUsageRouteStub(t, warpapi.ResponseEvent_StreamFinished_builder{RequestCharges: charges}.Build(), nil)
		e, _, h := newWarpRouteE2E(t, stub, 1)
		capture := &warpUsageAuditCapture{done: make(chan struct{})}
		h.SetAuditLogger(capture)
		resp := e.do(t, http.MethodPost, "/warp/v1/chat/completions", `{"model":"`+warpRouteModel+`","messages":[{"role":"user","content":"`+warpRouteQuery+`"}]}`, false)
		body := e.readBody(t, resp)
		if resp.StatusCode != 200 {
			t.Fatal("valid usage report failed")
		}
		usage := warpUsageFromWire(t, "chat/completions", false, body)
		event := capture.single(t)
		if partial {
			if _, exists := usage["prompt_tokens"]; exists {
				t.Fatal("unknown contributor input published as complete")
			}
			if event.UsageSource == audit.UsageSourceUpstream || event.PricingModel != "" || event.CostInUSDTicks != 0 {
				t.Fatal("partial contributors became complete billed usage")
			}
		} else {
			warpUsageWantCounter(t, usage, "prompt_tokens", 27022)
			warpUsageWantCounter(t, usage, "total_tokens", 27528)
			if event.InputTokens != 27022 || event.TotalTokens != 27528 || event.CacheWriteTokens != 0 {
				t.Fatal("observed zero write miscounted")
			}
		}
	}
}
