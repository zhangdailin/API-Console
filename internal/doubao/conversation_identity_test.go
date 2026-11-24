package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/middleware"
	"orchids-api/internal/prompt"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func TestConversationCacheRetainsActiveScopesAndEvictsStaleOnes(t *testing.T) {
	now := time.Now()
	cache := conversationCache[string]{limit: 2, ttl: time.Hour}
	cache.put("active", "a", now)
	cache.put("cold", "b", now)
	if got := cache.get("active", now.Add(time.Minute)); got != "a" {
		t.Fatalf("active conversation = %q", got)
	}
	cache.put("new", "c", now.Add(2*time.Minute))
	if got := cache.get("cold", now.Add(2*time.Minute)); got != "" {
		t.Fatalf("least recently used conversation survived capacity limit: %q", got)
	}
	if got := cache.get("active", now.Add(time.Hour)); got != "a" {
		t.Fatalf("active conversation did not slide expiration: %q", got)
	}
	if got := cache.get("new", now.Add(2*time.Hour)); got != "" || len(cache.entries) != 0 {
		t.Fatalf("expired conversations remain: %q (%d entries)", got, len(cache.entries))
	}
}

func dialogClient(t *testing.T, acc *store.Account, owner, dialog string) *Client {
	t.Helper()
	c := NewFromAccount(acc, nil)
	if err := c.bindConversationIdentity(middleware.WithAPIKeyFingerprint(t.Context(), owner), dialog); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDialogIdentityStableAcrossClientsAndCookieRefresh(t *testing.T) {
	acc := sessionTestAccount()
	acc.ID = 9401
	before := acc.Clone()
	a := dialogClient(t, acc, "caller-a", "dialog-a")
	b := dialogClient(t, acc, "caller-a", "dialog-a")
	if !reflect.DeepEqual(a.dialogIdentity, b.dialogIdentity) || !reflect.DeepEqual(acc, before) {
		t.Fatal("same dialog changed identity or mutated the account")
	}
	rotated := acc.Clone()
	rotated.ClientCookie = `{"sessionid":"synthetic-refreshed-cookie"}`
	refreshed := dialogClient(t, rotated, "caller-a", "dialog-a")
	if !reflect.DeepEqual(a.dialogIdentity, refreshed.dialogIdentity) {
		t.Fatal("ordinary Cookie refresh changed a named dialog identity")
	}
	for _, id := range []string{a.dialogIdentity.deviceID, a.dialogIdentity.webID, a.dialogIdentity.teaUUID} {
		if !regexp.MustCompile(`^[1-9][0-9]{18}$`).MatchString(id) {
			t.Fatalf("invalid identifier format: %q", id)
		}
	}
	if a.dialogIdentity.webID != a.dialogIdentity.teaUUID || a.dialogIdentity.deviceID == acc.DoubaoDeviceID {
		t.Fatal("dialog did not get its own complete identity tuple")
	}
	for _, secret := range []string{"caller-a", "dialog-a", "synthetic-old-session"} {
		if strings.Contains(a.dialogIdentity.scope, secret) {
			t.Fatal("conversation cache scope exposed caller data")
		}
	}
}

func TestDialogIdentityIsolatesCallerConversationAndLogin(t *testing.T) {
	acc := sessionTestAccount()
	acc.ID = 9402
	base := dialogClient(t, acc, "caller-a", "dialog-a").dialogIdentity
	otherLogin := acc.Clone()
	otherLogin.DoubaoDeviceID = "2000000000000000001"
	for _, c := range []*Client{
		dialogClient(t, acc, "caller-b", "dialog-a"),
		dialogClient(t, acc, "caller-a", "dialog-b"),
		dialogClient(t, otherLogin, "caller-a", "dialog-a"),
	} {
		if c.dialogIdentity.deviceID == base.deviceID || c.dialogIdentity.webID == base.webID || c.dialogIdentity.scope == base.scope {
			t.Fatal("different dialog, owner or login reused an identity")
		}
	}
	first, second := dialogClient(t, acc, "caller-a", ""), dialogClient(t, acc, "caller-a", "")
	if first.dialogIdentity.scope != "" || second.dialogIdentity.scope != "" || first.dialogIdentity.deviceID == second.dialogIdentity.deviceID {
		t.Fatal("anonymous independent requests reused an identity")
	}
}

func TestConcurrentDialogIdentityDerivationDoesNotMutateCachedClient(t *testing.T) {
	prototype := NewFromAccount(sessionTestAccount(), nil)
	results := make(chan conversationIdentity, 16)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			c := prototype.scopedClient()
			if err := c.bindConversationIdentity(context.Background(), "same-dialog"); err != nil {
				t.Error(err)
				return
			}
			results <- *c.dialogIdentity
		})
	}
	wg.Wait()
	close(results)
	var first *conversationIdentity
	for got := range results {
		if first == nil {
			copy := got
			first = &copy
		} else if *first != got {
			t.Fatal("concurrent requests assigned different tuples to one dialog")
		}
	}
	if prototype.dialogIdentity != nil {
		t.Fatal("request changed the shared cached client")
	}
}

func TestInferenceUsesAutomaticDialogIdentityThroughoutProtocol(t *testing.T) {
	s := sessionTestStore(t)
	acc := sessionTestAccount()
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetAccount(t.Context(), acc.ID)
	type sent struct{ conversation, device, web, tea string }
	var mu sync.Mutex
	created := map[string]sent{}
	var chats []sent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		row := sent{device: q.Get("device_id"), web: q.Get("web_id"), tea: q.Get("tea_uuid")}
		switch r.URL.Path {
		case "/im/conversation/create":
			var payload struct {
				Uplink struct {
					Create struct {
						Source string `json:"source_conversation_id"`
					} `json:"create_conv_uplink_body"`
				} `json:"uplink_body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			id := "synthetic-root"
			if payload.Uplink.Create.Source != "" {
				mu.Lock()
				id = fmt.Sprintf("synthetic-child-%d", len(created)+1)
				row.conversation = id
				created[id] = row
				mu.Unlock()
			}
			_, _ = fmt.Fprintf(w, `{"status_code":0,"downlink_body":{"create_conv_downlink_body":{"conversation_info":{"conversation_id":%q}}}}`, id)
		case "/chat/completion":
			var body struct {
				Meta struct {
					Conversation string `json:"conversation_id"`
				} `json:"client_meta"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			row.conversation = body.Meta.Conversation
			mu.Lock()
			if created[row.conversation] != row {
				t.Error("chat identity differs from the creation identity")
			}
			chats = append(chats, row)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: STREAM_MSG_NOTIFY\ndata: {\"content\":{\"content_block\":[{\"block_type\":10000,\"content\":{\"text_block\":{\"text\":\"synthetic answer\"}}}]}}\n\n")
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := &config.Config{DoubaoBaseURL: server.URL, DoubaoRequestGapMS: 1}
	for _, turn := range []struct{ owner, dialog string }{{"protocol-a", "alpha-9403"}, {"protocol-a", "alpha-9403"}, {"protocol-a", "beta-9403"}, {"protocol-b", "alpha-9403"}, {"protocol-a", ""}, {"protocol-a", ""}} {
		fresh, _ := s.GetAccount(t.Context(), acc.ID)
		c := newClientWithStore(fresh, cfg, s)
		ctx := middleware.WithAPIKeyFingerprint(t.Context(), turn.owner)
		req := upstream.UpstreamRequest{Model: "doubao", ConversationID: turn.dialog, Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "synthetic input"}}}}
		if err := c.SendRequestWithPayload(ctx, req, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(chats) != 6 || len(created) != 5 || chats[0] != chats[1] {
		t.Fatalf("dialog reuse failed: chats=%d created=%d", len(chats), len(created))
	}
	for _, index := range []int{2, 3, 4, 5} {
		if chats[index].device == chats[0].device || chats[index].conversation == chats[0].conversation {
			t.Fatal("independent dialog reused an upstream identity or conversation")
		}
	}
	if chats[4].device == chats[5].device || chats[4].conversation == chats[5].conversation {
		t.Fatal("anonymous requests reused each other's dialog")
	}
	after, _ := s.GetAccount(t.Context(), acc.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("automatic identity selection modified stored credentials, verdicts or account settings")
	}
}

func TestConversationChallengeKeepsRefusedDeviceAndAccountGeneration(t *testing.T) {
	acc := challengeTestAccount()
	acc.ID = 9404
	t.Cleanup(func() { clearChallenge(acc.ID) })
	a := dialogClient(t, acc, "caller-a", "alpha")
	b := dialogClient(t, acc, "caller-a", "beta")
	noteChallengeForRequest(acc, a.challengeAccount(), challengeTestError())
	challenge, ok := verification(acc)
	if !ok || challenge.Params["info"].(map[string]any)["did"] != a.dialogIdentity.deviceID {
		t.Fatal("SDK did not receive the exact refused dialog identity")
	}
	clearChallengeForRequest(acc, b.challengeAccount())
	if _, ok := verification(acc); !ok {
		t.Fatal("another dialog's success cleared the refusal")
	}
	rotated := acc.Clone()
	rotated.DoubaoDeviceID = "2000000000000000001"
	verificationSolved(rotated)
	if _, ok := verification(rotated); ok {
		t.Fatal("new login identity reused an old challenge")
	}
	if _, ok := verification(acc); !ok {
		t.Fatal("stale or unrelated acknowledgement cleared a challenge")
	}
	verificationSolved(acc)
	if _, ok := verification(acc); ok {
		t.Fatal("explicit acknowledgement failed to clear its challenge")
	}
	var required *verificationRequired
	if !errors.As(challengeTestError(), &required) {
		t.Fatal("challenge classification changed")
	}
}

func TestConcurrentNamedDialogCreatesOneUpstreamChild(t *testing.T) {
	acc := sessionTestAccount()
	acc.ID = 9405
	rootConversations.put(acc.ID, "synthetic-concurrent-root", time.Now())
	var creates atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		creates.Add(1)
		once.Do(func() { close(started) })
		<-release
		_, _ = io.WriteString(w, `{"status_code":0,"downlink_body":{"create_conv_downlink_body":{"conversation_info":{"conversation_id":"synthetic-single-child"}}}}`)
	}))
	defer server.Close()
	cfg := &config.Config{DoubaoBaseURL: server.URL}
	prototype := NewFromAccount(acc, cfg)
	ready, start := make(chan struct{}, 16), make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			c := prototype.scopedClient()
			if err := c.bindConversationIdentity(t.Context(), "concurrent-9405"); err != nil {
				t.Error(err)
				return
			}
			ready <- struct{}{}
			<-start
			id, err := c.conversationFor(t.Context(), c.jar())
			if err != nil || id != "synthetic-single-child" {
				t.Errorf("concurrent conversation: %q %v", id, err)
			}
		})
	}
	for range 16 {
		<-ready
	}
	close(start)
	<-started
	time.Sleep(10 * time.Millisecond)
	close(release)
	wg.Wait()
	if creates.Load() != 1 || prototype.dialogIdentity != nil {
		t.Fatalf("concurrent dialog created %d children or mutated the prototype", creates.Load())
	}
}

func TestRefusedDialogUsesItsOwnChallengeAndNeverAutomaticallyRetries(t *testing.T) {
	s := sessionTestStore(t)
	acc := sessionTestAccount()
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clearChallenge(acc.ID) })
	var chats atomic.Int32
	var mu sync.Mutex
	device := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completion" {
			_, _ = io.WriteString(w, `{"status_code":0,"downlink_body":{"create_conv_downlink_body":{"conversation_info":{"conversation_id":"synthetic-refused-child"}}}}`)
			return
		}
		chats.Add(1)
		mu.Lock()
		device = r.URL.Query().Get("device_id")
		mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: "synthetic-refreshed-after-refusal", Path: "/"})
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"code":710022004,"data":{"extra":{"decision":"{\"from\":\"shark_admin\",\"type\":\"verify\",\"subtype\":\"slide\",\"detail\":\"synthetic\"}"}}}`)
	}))
	defer server.Close()
	client := newClientWithStore(acc, &config.Config{DoubaoBaseURL: server.URL, DoubaoRequestGapMS: 1}, s)
	ctx := middleware.WithAPIKeyFingerprint(t.Context(), "refusal-owner")
	err := client.SendRequestWithPayload(ctx, upstream.UpstreamRequest{Model: "doubao", ConversationID: "refused-9406", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "synthetic input"}}}}, nil, nil)
	var required *verificationRequired
	if !errors.As(err, &required) || chats.Load() != 1 {
		t.Fatalf("refusal was replayed or misclassified: calls=%d err=%v", chats.Load(), err)
	}
	stored, _ := s.GetAccount(t.Context(), acc.ID)
	if stored.DoubaoDeviceID != acc.DoubaoDeviceID || stored.AuthStatus != acc.AuthStatus || decodeCookies(stored.ClientCookie)["sessionid"] != "synthetic-refreshed-after-refusal" {
		t.Fatal("dialog rotation changed the base tuple/verdict or lost the real Cookie rotation")
	}
	challenge, challengeErr := verificationFromStore(t.Context(), provider.Env{Store: s}, stored)
	mu.Lock()
	defer mu.Unlock()
	if challengeErr != nil || challenge.Params["info"].(map[string]any)["did"] != device || device == stored.DoubaoDeviceID {
		t.Fatal("SDK challenge did not preserve the refused dialog's actual device")
	}
	next := newClientWithStore(stored, nil, s)
	if err := next.bindConversationIdentity(ctx, "refused-9406"); err != nil || next.dialogIdentity.deviceID != device {
		t.Fatal("403 or Cookie rotation silently replaced the existing dialog identity")
	}
}

func TestParkedAccountCannotEscapeVerificationThroughANewDialog(t *testing.T) {
	s := sessionTestStore(t)
	acc := sessionTestAccount()
	acc.AuthStatus = store.AccountAuthStatusReauthRequired
	acc.StatusCode, acc.StatusMessage = "403", "requires human verification"
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clearChallenge(acc.ID) })
	noteChallenge(acc, challengeTestError())
	before, _ := verification(acc)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	c := newClientWithStore(acc, &config.Config{DoubaoBaseURL: server.URL, DoubaoRequestGapMS: 1}, s)
	err := c.SendRequestWithPayload(t.Context(), upstream.UpstreamRequest{Model: "doubao", ConversationID: "different-parked-dialog", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "synthetic"}}}}, nil, nil)
	if !errors.Is(err, errSessionChanged) || calls.Load() != 0 {
		t.Fatalf("new dialog escaped the existing account guard: calls=%d err=%v", calls.Load(), err)
	}
	stored, _ := s.GetAccount(t.Context(), acc.ID)
	after, ok := verification(stored)
	if !ok || !reflect.DeepEqual(before.Params, after.Params) || stored.DoubaoDeviceID != acc.DoubaoDeviceID || stored.AuthStatus != acc.AuthStatus {
		t.Fatal("new dialog changed a parked identity, verdict or pending challenge")
	}
}
