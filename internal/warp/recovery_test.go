package warp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func recoveryFixture(t *testing.T) (context.Context, callBinding, upstream.UpstreamRequest, streamResult) {
	t.Helper()
	ctx := bindingAuthContext(t, 91, "synthetic-recovery-owner")
	accumulator := newContinuationAccumulator(nil)
	continuationApply(t, accumulator, continuationCreate(continuationText("original", "synthetic original")))
	root, err := accumulator.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	binding := callBinding{ConversationID: "conv-recovery", RootTaskContext: root, ToolRounds: 1}
	req := historyRequest(textMessage("user", "echo"), prompt.Message{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "call-recovery", Name: "echo", Input: map[string]any{}}}}}, prompt.Message{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "call-recovery", Content: "synthetic-result"}}}})
	req.Tools = []any{map[string]any{"name": "echo", "input_schema": map[string]any{"type": "object"}}}
	continuationApply(t, accumulator, warpapi.ClientAction_builder{UpdateTaskServerData: warpapi.ClientAction_UpdateTaskServerData_builder{TaskId: proto.String("root"), ServerData: proto.String("accepted-failed-turn")}.Build()}.Build())
	checkpoint, err := accumulator.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return ctx, binding, req, streamResult{ConversationID: "conv-recovery", RecoverySafe: true, RecoveryContext: checkpoint}
}

func recoveryTestDigest(t *testing.T, req upstream.UpstreamRequest) string {
	t.Helper()
	prepared, err := validateRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := recoveryDigest(req, prepared)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func recoveryTestClient(b *bindingStore) *Client { return &Client{bindings: b} }

func TestRecoveryCASConcurrentHeadAndOriginalClaimPreserved(t *testing.T) {
	mini := miniredis.RunT(t)
	ctx, original, req, result := recoveryFixture(t)
	digest := recoveryTestDigest(t, req)
	first := recoveryTestClient(toolClaimRedisStore(t, mini))
	if err := first.bindings.claimToolTurn(ctx, 7, req.Model, []string{"call-recovery"}, original); err != nil {
		t.Fatal(err)
	}
	if !first.finishRecovery(ctx, 7, req.Model, []string{"call-recovery"}, digest, original, nil, nil, result, true, "optional-request-id") {
		t.Fatal("safe paid failure did not save checkpoint")
	}
	clients := []*Client{first, recoveryTestClient(toolClaimRedisStore(t, mini))}
	head, raw, err := first.loadRecovery(ctx, 7, req.Model, []string{"call-recovery"}, digest)
	if err != nil {
		t.Fatal(err)
	}
	const contenders = 24
	var winners atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			<-start
			if _, _, err := c.reserveRecovery(ctx, head, raw); err == nil {
				winners.Add(1)
			}
		}(clients[i%len(clients)])
	}
	close(start)
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("running head winners=%d, want 1", winners.Load())
	}
	if _, _, err := first.loadRecovery(ctx, 7, req.Model, []string{"call-recovery"}, digest); err == nil {
		t.Fatal("running head was reloadable")
	}
	if err := clients[1].bindings.claimToolTurn(ctx, 7, req.Model, []string{"call-recovery"}, original); err == nil {
		t.Fatal("resume cleared the original paid tool-result claim")
	}
}

func TestRecoveryAttemptQuotaAcrossCheckpointsAndConversationHeads(t *testing.T) {
	for _, redis := range []bool{false, true} {
		name := "local"
		if redis {
			name = "redis"
		}
		t.Run(name, func(t *testing.T) {
			ctx, original, req, result := recoveryFixture(t)
			c := recoveryTestClient(newBindingStore())
			if redis {
				c.bindings = toolClaimRedisStore(t, miniredis.RunT(t))
			}
			digest := recoveryTestDigest(t, req)
			ids := []string{"call-recovery"}
			if !c.finishRecovery(ctx, 7, req.Model, ids, digest, original, nil, nil, result, true, "") {
				t.Fatal("missing checkpoint")
			}
			for attempt := 0; attempt < recoveryAttemptLimit; attempt++ {
				head, raw, err := c.loadRecovery(ctx, 7, req.Model, ids, digest)
				if err != nil {
					t.Fatalf("load attempt %d: %v", attempt, err)
				}
				running, reserved, err := c.reserveRecovery(ctx, head, raw)
				if err != nil {
					t.Fatalf("reserve attempt %d: %v", attempt, err)
				}
				available := c.finishRecovery(ctx, 7, req.Model, ids, digest, original, &running, reserved, result, true, "")
				if available != (attempt+1 < recoveryAttemptLimit) {
					t.Fatal("wrong availability at attempt quota")
				}
			}
			if _, _, err := c.loadRecovery(ctx, 7, req.Model, ids, digest); err == nil {
				t.Fatal("fourth resume available")
			}
			// A new tool-result head must not reset the conversation's three attempts.
			newIDs := []string{"next-call"}
			if !c.finishRecovery(ctx, 7, req.Model, newIDs, digest, original, nil, nil, result, true, "") {
				t.Fatal("new failed head could not be persisted")
			}
			head, raw, err := c.loadRecovery(ctx, 7, req.Model, newIDs, digest)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := c.reserveRecovery(ctx, head, raw); err == nil {
				t.Fatal("new failed head reset conversation-wide resume quota")
			}
		})
	}
}

func TestRecoveryScopeDigestAndCheckpointValidation(t *testing.T) {
	ctx, original, req, result := recoveryFixture(t)
	c := recoveryTestClient(newBindingStore())
	digest := recoveryTestDigest(t, req)
	ids := []string{"call-recovery"}
	if !c.finishRecovery(ctx, 7, req.Model, ids, digest, original, nil, nil, result, true, "") {
		t.Fatal("missing checkpoint")
	}
	for _, tc := range []struct {
		ctx     context.Context
		account int64
		model   string
		ids     []string
		digest  string
	}{
		{context.Background(), 7, req.Model, ids, digest},
		{bindingAuthContext(t, 92, "synthetic-recovery-owner"), 7, req.Model, ids, digest},
		{bindingAuthContext(t, 91, "synthetic-recovery-rotated"), 7, req.Model, ids, digest},
		{ctx, 8, req.Model, ids, digest}, {ctx, 7, "model-b", ids, digest},
		{ctx, 7, req.Model, []string{"wrong-call"}, digest}, {ctx, 7, req.Model, ids, "changed-digest"},
		{ctx, 7, req.Model, []string{"call-recovery", "call-recovery"}, digest},
	} {
		if _, _, err := c.loadRecovery(tc.ctx, tc.account, tc.model, tc.ids, tc.digest); err == nil {
			t.Fatal("scope, digest, or result mismatch accepted")
		}
	}
	for _, mutation := range []func(*upstream.UpstreamRequest){
		func(r *upstream.UpstreamRequest) { r.System = []prompt.SystemItem{{Text: "changed instructions"}} },
		func(r *upstream.UpstreamRequest) {
			r.Tools = []any{map[string]any{"name": "echo", "description": "changed", "input_schema": map[string]any{"type": "object"}}}
		},
		func(r *upstream.UpstreamRequest) {
			r.Messages = append([]prompt.Message(nil), r.Messages...)
			r.Messages[2].Content.Blocks = append([]prompt.ContentBlock(nil), r.Messages[2].Content.Blocks...)
			r.Messages[2].Content.Blocks[0].Content = "changed result"
		},
	} {
		changed := req
		mutation(&changed)
		if recoveryTestDigest(t, changed) == digest {
			t.Fatal("changed body not bound by recovery digest")
		}
	}
	for _, unsafe := range []streamResult{
		{}, {RecoverySafe: true, ConversationID: "foreign", RecoveryContext: result.RecoveryContext},
		{RecoverySafe: true, ConversationID: original.ConversationID, RecoveryContext: []byte("invalid")},
	} {
		other := recoveryTestClient(newBindingStore())
		if other.finishRecovery(ctx, 7, req.Model, ids, digest, original, nil, nil, unsafe, true, "") {
			t.Fatal("unsafe checkpoint persisted")
		}
	}
	// Final success invalidates an already claimed head even if parser fields exist.
	head, raw, _ := c.loadRecovery(ctx, 7, req.Model, ids, digest)
	running, reserved, err := c.reserveRecovery(ctx, head, raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.finishRecovery(ctx, 7, req.Model, ids, digest, original, &running, reserved, result, false, "") {
		t.Fatal("success remained recoverable")
	}
	if _, _, err := c.loadRecovery(ctx, 7, req.Model, ids, digest); err == nil {
		t.Fatal("successful head reloaded")
	}
}

func TestRecoveryRedisPaidLimitExpiryAndStoreFailures(t *testing.T) {
	mini := miniredis.RunT(t)
	ctx, original, req, result := recoveryFixture(t)
	c := recoveryTestClient(toolClaimRedisStore(t, mini))
	digest := recoveryTestDigest(t, req)
	ids := []string{"call-recovery"}
	if !c.finishRecovery(ctx, 7, req.Model, ids, digest, original, nil, nil, result, true, "") {
		t.Fatal("missing checkpoint")
	}
	head, raw, _ := c.loadRecovery(ctx, 7, req.Model, ids, digest)
	attempts, paid := recoveryCounterKeys(c.bindings, head)
	mini.Set(paid, "6")
	if _, _, err := c.reserveRecovery(ctx, head, raw); err == nil {
		t.Fatal("resume exceeded paid conversation limit")
	}
	if mini.Exists(attempts) {
		t.Fatal("rejected paid reservation consumed resume quota")
	}
	mini.Del(paid)
	for _, key := range mini.Keys() {
		if strings.Contains(key, "conv-recovery") || strings.Contains(key, "call-recovery") || mini.TTL(key) != recoveryTTL {
			t.Fatal("recovery key or TTL leaked identity or exceeded contract")
		}
	}
	mini.FastForward(recoveryTTL + time.Second)
	if _, _, err := c.loadRecovery(ctx, 7, req.Model, ids, digest); err == nil {
		t.Fatal("expired checkpoint accepted")
	}
	bad := recoveryTestClient(newBindingStore())
	bad.bindings.redis = &continuationMemoryRedis{}
	if bad.finishRecovery(ctx, 7, req.Model, ids, digest, original, nil, nil, result, true, "") {
		t.Fatal("non-atomic backend fell back to local recovery")
	}
}

func TestRecoveryClientRejectsUndeclaredToolInFailedCheckpoint(t *testing.T) {
	ctx, original, req, _ := recoveryFixture(t)
	s := catalogRotationStore(t)
	account := testAccount()
	if err := s.CreateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	call := mcpToolCallEvent(t, "foreign-call", "undeclared", map[string]any{})
	message := call.GetClientActions().GetActions()[0].GetAddMessagesToTask().GetMessages()[0]
	message.SetTaskId("root")
	message.GetToolCall().GetCallMcpTool().SetServerId("client-request-tools")
	stream := integrityStream(t, initEvent(t, original.ConversationID), call)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer server.Close()
	client := newClientWithStore(account, testConfig(server.URL), s)
	if !client.bindings.saveScoped(ctx, account.ID, req.Model, "call-recovery", original) {
		t.Fatal("binding seed failed")
	}
	if err := client.SendRequestWithPayload(ctx, req, nil, nil); err == nil || strings.Contains(err.Error(), "explicit resume is available") {
		t.Fatalf("undeclared tool became recoverable: %v", err)
	}
	if err := client.SendRequestWithPayload(upstream.WithExplicitResume(ctx, true), req, nil, nil); err == nil || calls.Load() != 1 {
		t.Fatal("undeclared failed tool checkpoint reached upstream")
	}
}

func TestRecoveryClientExplicitResumeUsesOfficialInputOnly(t *testing.T) {
	ctx, original, req, _ := recoveryFixture(t)
	s := catalogRotationStore(t)
	account := testAccount()
	if err := s.CreateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	wires := make(chan *warpapi.Request, 3)
	failed := integrityStream(t, initEvent(t, original.ConversationID), testAction(warpapi.ClientAction_builder{UpdateTaskServerData: warpapi.ClientAction_UpdateTaskServerData_builder{TaskId: proto.String("root"), ServerData: proto.String("accepted-failed-turn")}.Build()}.Build()))
	parsed, parseErr := readStreamWithContext(strings.NewReader(failed), req.Model, original.RootTaskContext)
	if parseErr == nil || !parsed.RecoverySafe {
		t.Fatalf("fixture checkpoint not recoverable: err=%v safe=%v events=%d failure=%+v", parseErr, parsed.RecoverySafe, parsed.Events, parsed.Failure)
	}
	success := integrityStream(t, initEvent(t, original.ConversationID), integrityTaskMessages(continuationText("recovered", "recovered answer")), testDone())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn := calls.Add(1)
		if turn > 3 {
			t.Error("unexpected extra paid call")
			w.WriteHeader(500)
			return
		}
		raw, err := io.ReadAll(r.Body)
		wire := &warpapi.Request{}
		if err != nil || proto.Unmarshal(raw, wire) != nil {
			t.Error("invalid protobuf")
			w.WriteHeader(400)
			return
		}
		wires <- wire
		w.Header().Set("Content-Type", "text/event-stream")
		if turn < 3 {
			_, _ = io.WriteString(w, failed)
		} else {
			_, _ = io.WriteString(w, success)
		}
	}))
	defer server.Close()
	client := newClientWithStore(account, testConfig(server.URL), s)
	if !client.bindings.saveScoped(ctx, account.ID, req.Model, "call-recovery", original) {
		t.Fatal("binding seed failed")
	}
	emitted := 0
	err := client.SendRequestWithPayload(ctx, req, func(upstream.SSEMessage) { emitted++ }, nil)
	var app *apperrors.AppError
	if !errors.As(err, &app) || app.HTTPStatus != 502 || !strings.Contains(app.Message, "X-Warp-Resume: true") || emitted != 0 || calls.Load() != 1 {
		t.Fatalf("failed tool continuation did not offer explicit recovery: %v", err)
	}
	originalWire := <-wires
	if len(originalWire.GetInput().GetUserInputs().GetInputs()) != 1 || originalWire.GetInput().GetUserInputs().GetInputs()[0].GetToolCallResult() == nil {
		t.Fatal("first continuation did not send typed result")
	}
	if err := client.SendRequestWithPayload(ctx, req, nil, nil); err == nil || calls.Load() != 1 {
		t.Fatal("ordinary replay bypassed original result claim")
	}
	// Neither opt-in nor a known call ID can substitute ownership or body.
	for _, mutation := range []func(*upstream.UpstreamRequest){
		func(r *upstream.UpstreamRequest) { r.System = []prompt.SystemItem{{Text: "changed"}} },
		func(r *upstream.UpstreamRequest) {
			r.Tools = []any{map[string]any{"name": "echo", "description": "changed", "input_schema": map[string]any{"type": "object"}}}
		},
	} {
		changed := req
		mutation(&changed)
		if err := client.SendRequestWithPayload(upstream.WithExplicitResume(ctx, true), changed, nil, nil); err == nil || calls.Load() != 1 {
			t.Fatal("changed resume body reached upstream")
		}
	}
	if err := client.SendRequestWithPayload(upstream.WithExplicitResume(context.Background(), true), req, nil, nil); err == nil || calls.Load() != 1 {
		t.Fatal("raw resume opt-in bypassed authentication")
	}
	// Recreate the client: checkpoints must be persisted, not process-local.
	restarted := newClientWithStore(account, testConfig(server.URL), s)
	var events []upstream.SSEMessage
	resumeCtx := upstream.WithExplicitResume(ctx, true)
	if err := restarted.SendRequestWithPayload(resumeCtx, req, nil, nil); !errors.As(err, &app) || !strings.Contains(app.Message, "X-Warp-Resume: true") {
		t.Fatalf("failed resume did not replace checkpoint: %v", err)
	}
	failedResumeWire := <-wires
	if failedResumeWire.GetInput().GetResumeConversation() == nil || failedResumeWire.GetInput().GetUserInputs() != nil {
		t.Fatal("first explicit resume replayed result inputs")
	}
	restarted = newClientWithStore(account, testConfig(server.URL), s)
	if err := restarted.SendRequestWithPayload(resumeCtx, req, func(e upstream.SSEMessage) { events = append(events, e) }, nil); err != nil {
		t.Fatalf("explicit resume failed: %v", err)
	}
	resumedWire := <-wires
	if resumedWire.GetInput().GetResumeConversation() == nil || resumedWire.GetInput().GetUserInputs() != nil {
		t.Fatal("resume resent tool results or user query instead of ResumeConversation")
	}
	tasks := resumedWire.GetTaskContext().GetTasks()
	if len(tasks) != 1 || tasks[0].GetId() != "root" || tasks[0].GetServerData() != "accepted-failed-turn" || resumedWire.GetMetadata().GetConversationId() != original.ConversationID {
		t.Fatal("resume lost current accepted checkpoint or conversation identity")
	}
	if len(events) != 2 || events[0].Event["delta"] != "recovered answer" {
		t.Fatalf("unexpected resumed events: %+v", events)
	}
	for _, explicit := range []bool{false, true} {
		if err := restarted.SendRequestWithPayload(upstream.WithExplicitResume(ctx, explicit), req, nil, nil); err == nil || calls.Load() != 3 {
			t.Fatal("completed or consumed request was replayed")
		}
	}
	raw, _ := json.Marshal(req)
	if !bytes.Contains(raw, []byte("call-recovery")) {
		t.Fatal("original body mutated")
	}
}
