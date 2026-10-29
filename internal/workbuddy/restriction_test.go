package workbuddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestRestrictionPersistsAcrossClientsAndSkipsAllEntrances(t *testing.T) {
	for _, entrance := range []string{"chat", "catalog", "quota", "stream"} {
		t.Run(entrance, func(t *testing.T) {
			ctx := context.Background()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if entrance == "stream" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"code\":11140,\"msg\":\"request illegal\"}\n\n"))
					return
				}
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"code":11140,"msg":"request illegal"}`))
			}))
			defer srv.Close()
			mini := miniredis.RunT(t)
			s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "restriction:"})
			testutil.NoError(t, err)
			defer s.Close()
			acc := &store.Account{AccountType: "workbuddy", Enabled: true, WorkBuddyAccessToken: "access", WorkBuddyUID: "uid"}
			testutil.NoError(t, s.CreateAccount(ctx, acc))
			stale := *acc
			cfg := &config.Config{WorkBuddyBaseURL: srv.URL}
			client := newClientWithStore(acc, cfg, s)
			if entrance == "catalog" {
				_, err = client.FetchModels(ctx)
			} else if entrance == "quota" {
				_, err = client.FetchQuota(ctx)
			} else {
				err = client.runChat(ctx, upstream.UpstreamRequest{Model: "m"}, time.Second, nil, nil)
			}
			if err == nil {
				t.Fatal("restriction must fail")
			}
			saved, err := s.GetAccount(ctx, acc.ID)
			testutil.NoError(t, err)
			testutil.Equal(t, saved.StatusCode, "403")
			until := saved.WorkBuddyRestrictedUntil
			if !until.After(time.Now()) {
				t.Fatal("restriction deadline was not persisted")
			}

			// A client constructed from a pre-failure snapshot must consult the
			// persisted verdict before making even a token-refresh request.
			other := newClientWithStore(&stale, cfg, s)
			_, err = other.FetchModels(ctx)
			if err == nil {
				t.Fatal("catalog must skip a restricted account")
			}
			_, err = other.FetchQuota(ctx)
			if err == nil {
				t.Fatal("quota must skip a restricted account")
			}
			err = other.runChat(ctx, upstream.UpstreamRequest{Model: "m"}, time.Second, nil, nil)
			if err == nil {
				t.Fatal("chat must skip a restricted account")
			}
			_, status, err := VerifyAccount(ctx, &stale, cfg, s)
			testutil.Equal(t, status, http.StatusForbidden)
			if err == nil {
				t.Fatal("manual verification must not report success")
			}
			maintain(ctx, provider.Env{Config: cfg, Store: s}, saved)
			testutil.False(t, catalogRefreshDue(saved, time.Now()))
			testutil.False(t, quotaRefreshDue(saved, time.Now()))
			testutil.True(t, catalogRefreshDue(saved, until.Add(time.Second)))
			testutil.True(t, quotaRefreshDue(saved, until.Add(time.Second)))
			testutil.Equal(t, calls.Load(), int32(1))
			after, err := s.GetAccount(ctx, acc.ID)
			testutil.NoError(t, err)
			testutil.Equal(t, after.WorkBuddyRestrictedUntil, until)
		})
	}
}
