package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestWorkBuddy11140PersistsAndReportsRestrictionOnFollowingRequests(t *testing.T) {
	s := newTestRedisStore(t, "wb-restricted-handler:")
	acc := &store.Account{Name: "wb", AccountType: "workbuddy", RefreshToken: "rt", Enabled: true, Weight: 1}
	testutil.NoError(t, s.CreateAccount(context.Background(), acc))
	publishModel(t, s, &store.Model{Channel: "WorkBuddy", ModelID: "claude-opus-5"})
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, loadbalancer.NewWithCacheTTL(s, 0))
	calls := 0
	h.SetClientFactory(func(*store.Account, *config.Config) UpstreamClient {
		calls++
		return &errorUpstreamEdge{err: errors.New(store.WorkBuddyRestrictionMessage)}
	})
	for range 2 {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", bytes.NewBufferString(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":false}`))
		h.HandleMessages(w, r)
		testutil.Equal(t, w.Code, http.StatusForbidden)
		testutil.MustContain(t, w.Body.String(), "account_restricted")
	}
	testutil.Equal(t, calls, 1)
	saved, err := s.GetAccount(context.Background(), acc.ID)
	testutil.NoError(t, err)
	testutil.False(t, saved.WorkBuddyRestrictedUntil.IsZero())
}
