package warp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func TestFailedPersistenceRecoversRotationAcrossFreshClients(t *testing.T) {
	var refreshCalls, paidCalls atomic.Int64
	stream := testSSE(t, testMessages(testOutput("recovery", "recovered answer"))) + testSSE(t, testDone())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/token":
			refreshCalls.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "synthetic-refresh" {
				t.Error("refresh did not use the original grant")
			}
			_, _ = w.Write([]byte(`{"id_token":"synthetic-renewed","refresh_token":"synthetic-rotated","expires_in":"3600"}`))
		case "/ai/multi-agent":
			paidCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer synthetic-renewed" {
				t.Error("generation did not use the recovered access token")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(stream))
		default:
			t.Error("unexpected network request")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	acc := testAccount()
	acc.ID = 101
	acc.WarpExpiresAt = time.Now().Add(-time.Hour)
	owner := &flakyCredentialStore{acc: *acc, fail: true}
	key := rotationKey(owner, acc.ID, acc.WarpRefreshToken)
	t.Cleanup(func() { forgetRotation(key) })
	first := NewFromAccount(acc, testConfig(server.URL))
	first.SetAccountStore(owner)
	if err := first.SendRequestWithPayload(context.Background(), testRequest(), nil, nil); !errors.Is(err, errPersistence) {
		t.Fatalf("first generation error = %v, want persistence failure", err)
	}
	if refreshCalls.Load() != 1 || paidCalls.Load() != 0 || owner.calls != 1 {
		t.Fatalf("failed commit sent unexpected calls: refresh=%d paid=%d commits=%d", refreshCalls.Load(), paidCalls.Load(), owner.calls)
	}
	if owner.acc.WarpRefreshToken != acc.WarpRefreshToken || owner.acc.WarpAccessToken != acc.WarpAccessToken {
		t.Fatal("failed commit changed the authoritative credentials")
	}

	// A newly constructed client has neither the first client's token nor its state.
	fresh := NewFromAccount(acc, testConfig(server.URL))
	fresh.SetAccountStore(owner)
	if err := fresh.SendRequestWithPayload(context.Background(), testRequest(), nil, nil); err != nil {
		t.Fatalf("fresh client did not recover the pending rotation: %v", err)
	}
	if refreshCalls.Load() != 1 || paidCalls.Load() != 1 || owner.calls != 2 {
		t.Fatalf("rotation was replayed or not committed: refresh=%d paid=%d commits=%d", refreshCalls.Load(), paidCalls.Load(), owner.calls)
	}
	if owner.acc.WarpAccessToken != "synthetic-renewed" || owner.acc.WarpRefreshToken != "synthetic-rotated" || !owner.acc.WarpExpiresAt.After(time.Now().Add(time.Minute)) {
		t.Fatal("recovered grant was not persisted")
	}
	if _, pending := recoverRotation(key, time.Now()); pending {
		t.Fatal("successful recovery left a pending rotation behind")
	}
}

// authRecoveryStore can change the authoritative result between token lookup and
// the final generation authorization without mutating the client's cached account.
type authRecoveryStore struct {
	initial  *store.Account
	latest   *store.Account
	switchAt int
	reads    int
	writes   int
}

func (s *authRecoveryStore) GetAccount(context.Context, int64) (*store.Account, error) {
	s.reads++
	acc := s.initial
	if s.reads >= s.switchAt {
		acc = s.latest
	}
	if acc == nil {
		return nil, nil
	}
	copy := *acc
	copy.WarpModelIDs = append([]string(nil), acc.WarpModelIDs...)
	return &copy, nil
}

func (s *authRecoveryStore) UpdateWarpCredentials(context.Context, int64, store.WarpCredentialPatch) error {
	s.writes++
	return errors.New("unexpected credential write")
}

func TestGenerationRejectsDisabledOrDeletedAuthoritativeAccount(t *testing.T) {
	for _, state := range []string{"disabled", "deleted"} {
		for _, switchAt := range []int{1, 2} {
			stage := "before_token_lookup"
			if switchAt == 2 {
				stage = "before_generation"
			}
			t.Run(state+"/"+stage, func(t *testing.T) {
				var networkCalls, paidCalls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					networkCalls.Add(1)
					if r.URL.Path == "/ai/multi-agent" {
						paidCalls.Add(1)
					}
					w.WriteHeader(http.StatusInternalServerError)
				}))
				defer server.Close()
				acc := testAccount()
				acc.ID = 102
				if !credentials(acc).valid() {
					t.Fatal("test requires a valid cached access token")
				}
				var latest *store.Account
				if state == "disabled" {
					copy := *acc
					copy.Enabled = false
					latest = &copy
				}
				owner := &authRecoveryStore{initial: acc, latest: latest, switchAt: switchAt}
				c := NewFromAccount(acc, testConfig(server.URL))
				c.SetAccountStore(owner)
				ctx, budget := upstream.WithAttemptBudget(context.Background(), 1)
				emitted := false
				err := c.SendRequestWithPayload(ctx, testRequest(), func(upstream.SSEMessage) { emitted = true }, nil)
				if err == nil {
					t.Fatal("generation accepted an unavailable authoritative account")
				}
				if owner.reads != switchAt || owner.writes != 0 {
					t.Fatalf("unexpected authoritative store calls: reads=%d writes=%d", owner.reads, owner.writes)
				}
				if networkCalls.Load() != 0 || paidCalls.Load() != 0 || budget.Used() != 0 || emitted {
					t.Fatalf("unavailable account reached generation: network=%d paid=%d attempts=%d emitted=%t", networkCalls.Load(), paidCalls.Load(), budget.Used(), emitted)
				}
			})
		}
	}
}

func TestGenerationWithExhaustedAttemptBudgetDoesNotSend(t *testing.T) {
	var networkCalls, paidCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		networkCalls.Add(1)
		if r.URL.Path == "/ai/multi-agent" {
			paidCalls.Add(1)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	ctx, budget := upstream.WithAttemptBudget(context.Background(), 1)
	finish, err := upstream.BeginAttempt(ctx)
	if err != nil {
		t.Fatalf("consume attempt budget: %v", err)
	}
	finish(nil)
	if budget.Remaining() != 0 {
		t.Fatal("test did not exhaust the attempt budget")
	}
	acc := testAccount()
	acc.ID = 103
	owner := &flakyCredentialStore{acc: *acc}
	c := NewFromAccount(acc, testConfig(server.URL))
	c.SetAccountStore(owner)
	emitted := false
	err = c.SendRequestWithPayload(ctx, testRequest(), func(upstream.SSEMessage) { emitted = true }, nil)
	if !errors.Is(err, upstream.ErrAttemptBudget) {
		t.Fatalf("generation error = %v, want exhausted attempt budget", err)
	}
	if networkCalls.Load() != 0 || paidCalls.Load() != 0 || owner.calls != 0 || budget.Used() != 1 || emitted {
		t.Fatalf("exhausted budget sent a request: network=%d paid=%d commits=%d attempts=%d emitted=%t", networkCalls.Load(), paidCalls.Load(), owner.calls, budget.Used(), emitted)
	}
}
