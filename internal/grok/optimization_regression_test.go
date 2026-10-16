package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/redis/go-redis/v9"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/store"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOptimizationStreamingDurationUsesFirstAndLastDelta(t *testing.T) {
	for _, tc := range []struct {
		name        string
		first, last int64
		terminal    bool
		want        qualityVerdict
	}{
		{"fast first token and long stream", 500, 10000, true, qualityDeliver},
		{"slow first token and actual dump", 10000, 10010, true, qualityWithhold},
		{"one delta cannot establish a dump", 500, 500, false, qualityWait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := chatOutcome{Quality: qualitySignals{ExpectReasoning: true, VisibleChars: 200, EncryptedChars: 512, ReasoningTokens: 100}}
			result.Quality.observeVisible(tc.first)
			result.Quality.observeVisible(tc.last)
			hold := newBuildQualityHold(httptest.NewRecorder(), qualityHoldPolicy{}, &result)
			verdict, _ := hold.step(tc.terminal)
			if verdict != tc.want || hold.signals(tc.terminal).VisibleFlushMS != tc.last-tc.first {
				t.Fatalf("verdict=%s want=%s duration=%d", verdict, tc.want, hold.signals(tc.terminal).VisibleFlushMS)
			}
		})
	}
}

func TestOptimizationPayloadCloneSharesStringsAndIsolatesContainers(t *testing.T) {
	text := strings.Repeat("history", 1<<17)
	original := map[string]any{"input": []any{map[string]any{"content": text}}, "tools": []map[string]any{{"name": "lookup", "parameters": map[string]any{"type": "object"}}}, "include": []string{"reasoning.encrypted_content"}, "reasoning": map[string]any{"effort": "high"}, "large_id": uint64(9007199254740993)}
	copy, err := cloneBuildPayload(original)
	if err != nil {
		t.Fatal(err)
	}
	copy["input"].([]any)[0].(map[string]any)["content"] = "changed"
	copy["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)["type"] = "changed"
	copy["include"].([]any)[0] = "changed"
	copy["reasoning"].(map[string]any)["effort"] = "low"
	if original["input"].([]any)[0].(map[string]any)["content"] != text || original["tools"].([]map[string]any)[0]["parameters"].(map[string]any)["type"] != "object" || original["include"].([]string)[0] != "reasoning.encrypted_content" || original["reasoning"].(map[string]any)["effort"] != "high" {
		t.Fatal("copy mutated immutable history")
	}
	if copy["large_id"] != original["large_id"] {
		t.Fatal("integer precision lost")
	}
	for _, bad := range []any{math.NaN(), math.Inf(1), json.Number("bad"), make(chan int)} {
		if _, err := cloneBuildPayload(map[string]any{"bad": bad}); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	if _, err := cloneBuildPayload(cycle); err == nil {
		t.Fatal("cycle accepted")
	}
	raw := map[string]any{"extension": struct {
		Value string `json:"value"`
	}{"kept"}, "raw": json.RawMessage(`{"v":1}`)}
	cloned, err := cloneBuildPayload(raw)
	if err != nil || cloned["extension"].(map[string]any)["value"] != "kept" {
		t.Fatalf("custom JSON compatibility lost: %v", err)
	}
}

func TestOptimizationSessionsOwnAccountCollections(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	h, acc := parityBuildHandler(t, upstream)
	acc.ID, acc.TeamID = 0, t.Name()
	acc.GrokModels = []string{"grok-4.7"}
	acc.GrokModelsSyncedAt = time.Now()
	acc.ModelCooldowns = map[string]time.Time{"other": time.Now().Add(time.Hour)}
	ctx := context.Background()
	if err := h.lb.Store.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	first, err := h.openCLIAccountSession(ctx, nil, "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := h.openCLIAccountSession(ctx, nil, "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.acc == second.acc {
		t.Fatal("sessions share mutable account")
	}
	first.acc.ModelCooldowns["grok-4.7"] = time.Now().Add(time.Hour)
	first.acc.GrokModels[0] = "changed"
	if second.acc.GrokModels[0] != "grok-4.7" || !second.acc.ModelCooldowns["grok-4.7"].IsZero() {
		t.Fatal("sessions share account collections")
	}
	next, err := h.openCLIAccountSession(ctx, nil, "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

type cooldownIOHook struct{ pipelines, commands atomic.Int64 }

func (h *cooldownIOHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}
func (h *cooldownIOHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "pttl" {
			h.commands.Add(1)
		}
		return next(ctx, cmd)
	}
}
func (h *cooldownIOHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		cooldown := false
		for _, cmd := range cmds {
			if cmd.Name() == "pttl" {
				h.commands.Add(1)
				cooldown = true
			}
		}
		if cooldown {
			h.pipelines.Add(1)
		}
		return next(ctx, cmds)
	}
}

func TestOptimizationCooldownBatchDeduplicatesAndCancels(t *testing.T) {
	backend := withTestDistributedGrokLimits(t)
	if err := backend.client.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	hook := &cooldownIOHook{}
	backend.client.AddHook(hook)
	teamID := t.Name()
	t.Cleanup(func() {
		teamCooldown.mu.Lock()
		defer teamCooldown.mu.Unlock()
		for _, q := range identityCooldownQueries(ProviderBuild+":team:"+teamID, "grok-4.7") {
			delete(teamCooldown.entries, teamCooldownKey(q.scope, q.identity, q.model))
		}
	})
	snapshot := &cooldownSnapshot{}
	var queries []cooldownQuery
	for i := 0; i < 23; i++ {
		queries = append(queries, cooldownQueries(context.Background(), &store.Account{ID: int64(i + 1), TeamID: teamID}, "grok-4.7")...)
	}
	if err := snapshot.prepare(context.Background(), queries); err != nil {
		t.Fatal(err)
	}
	if hook.pipelines.Load() != 1 || hook.commands.Load() != 4 {
		t.Fatalf("not batched/deduplicated: pipelines=%d commands=%d", hook.pipelines.Load(), hook.commands.Load())
	}
	if err := snapshot.prepare(context.Background(), queries); err != nil {
		t.Fatal(err)
	}
	if hook.pipelines.Load() != 1 {
		t.Fatal("fallback repeated selection queries")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := scopedRateLimitCooldown(canceled, ProviderBuild, "token", "grok-4.7"); err != context.Canceled {
		t.Fatalf("cancellation lost: %v", err)
	}
	if hook.pipelines.Load() != 1 || hook.commands.Load() != 4 {
		t.Fatal("canceled request performed Redis IO")
	}
	if err := snapshot.check(context.Background(), queries); err != nil {
		t.Fatalf("negative snapshot was not clear: %v", err)
	}
	// Dispatch must not reuse the negative selection snapshot.
	teamCooldown.Note(RateLimitScopeRPM, ProviderBuild+":team:"+teamID, "grok-4.7", time.Minute)
	fresh := withRateLimitAccount(context.Background(), &store.Account{TeamID: teamID})
	if err := scopedRateLimitCooldown(fresh, ProviderBuild, "", "grok-4.7"); err == nil {
		t.Fatal("fresh cooldown bypassed at dispatch")
	}
}

func BenchmarkBuildPayloadClone(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20, 4 << 20} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			payload := map[string]any{"model": "grok-4.7", "input": []any{map[string]any{"role": "user", "content": strings.Repeat("a", size)}}}
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				copy, err := cloneBuildPayload(payload)
				if err != nil {
					b.Fatal(err)
				}
				if reflect.ValueOf(copy).IsNil() {
					b.Fatal("nil clone")
				}
			}
		})
	}
}

func TestOptimizationAccountSelectionUsesOneCooldownPipeline(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	h, base := parityBuildHandler(t, upstream)
	ctx := context.Background()
	for i := 0; i < 23; i++ {
		acc := base.Clone()
		acc.ID = 0
		acc.TeamID = fmt.Sprintf("%s-%d", t.Name(), i)
		acc.GrokModels = []string{"grok-4.7"}
		acc.GrokModelsSyncedAt = time.Now()
		if err := h.lb.Store.CreateAccount(ctx, acc); err != nil {
			t.Fatal(err)
		}
	}
	hook := &cooldownIOHook{}
	h.lb.Store.RedisClient().AddHook(hook)
	session, err := h.openCLIAccountSession(ctx, nil, "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	session.Close()
	if hook.pipelines.Load() != 1 || hook.commands.Load() != 92 {
		t.Fatalf("account scan not batched: pipelines=%d pttl=%d", hook.pipelines.Load(), hook.commands.Load())
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := h.openCLIAccountSession(canceled, nil, "grok-4.7"); err != context.Canceled {
		t.Fatalf("selection ignored cancellation: %v", err)
	}
	if hook.pipelines.Load() != 1 || hook.commands.Load() != 92 {
		t.Fatal("canceled selection performed cooldown reads")
	}
}

func TestPromptBilledSingleDeltaIsNotDegraded(t *testing.T) {
	for _, bill := range []int64{73, 82, 120} {
		result := chatOutcome{Quality: qualitySignals{ExpectReasoning: true, Terminal: true, VisibleChars: 20, EncryptedChars: 600, ReasoningTokens: bill}}
		result.Quality.observeVisible(1244)
		hold := newBuildQualityHold(httptest.NewRecorder(), qualityHoldPolicy{}, &result)
		verdict, _ := hold.step(true)
		if verdict != qualityDeliver || qualityDegraded(result.Quality) {
			t.Fatalf("healthy short answer rejected: bill=%d verdict=%s", bill, verdict)
		}
	}
}
