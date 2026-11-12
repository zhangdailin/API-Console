package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestParseSSOImportTokensAcceptsEveryImportShape(t *testing.T) {
	text := "tok-aaaaaaaaaaaaaaaa\n# comment\nsso=tok-bbbbbbbbbbbbbbbb; sso-rw=other\n\ntok-aaaaaaaaaaaaaaaa\ntok-c1,tok-c2"
	got := parseSSOImportTokens(text, []string{"sso=tok-d"})
	testutil.Equal(t, strings.Join(got, ","), "tok-d,tok-aaaaaaaaaaaaaaaa,tok-bbbbbbbbbbbbbbbb,tok-c1,tok-c2")

	grok2apiExport := `{"ssoBasic":[{"token":"tok-e","note":"x"}],"ssoSuper":["tok-f"]}`
	got = parseSSOImportTokens(grok2apiExport, nil)
	testutil.Equal(t, len(got), 2)
	testutil.MustContain(t, strings.Join(got, ","), "tok-e")
	testutil.MustContain(t, strings.Join(got, ","), "tok-f")

	testutil.Equal(t, len(parseSSOImportTokens("not a token with spaces", nil)), 0)
}

func TestPrepareAccountStoresOnlyTheCookieAndNeverNamesIt(t *testing.T) {
	const token = "short-secret-token"
	acc := &store.Account{AccountType: "grok", ClientCookie: "sso=" + token + "; sso-rw=x", OAuthRefreshToken: "stale"}
	testutil.NoError(t, PrepareAccount(acc))
	testutil.Equal(t, acc.ClientCookie, "sso="+token)
	testutil.Equal(t, acc.GrokProvider, ProviderWeb)
	testutil.Equal(t, acc.CredentialType, "sso")
	testutil.Equal(t, acc.OAuthRefreshToken, "")
	testutil.CheckNotContain(t, acc.Name, token)
	testutil.CheckNotContain(t, maskToken(token), token)
	testutil.True(t, IsWebAccount(acc))
	testutil.True(t, acc.IsGrokWeb())

	missing := &store.Account{AccountType: "grok", CredentialType: "sso"}
	testutil.Equal(t, PrepareAccount(missing), ErrMissingSSOToken)

	build := &store.Account{AccountType: "grok", CredentialType: "oauth", OAuthRefreshToken: "r"}
	testutil.False(t, IsWebAccount(build))
	testutil.False(t, LooksLikeWebAccount(build))
}

func webAccount(id int64, sub string, auto, fast bool) *store.Account {
	acc := &store.Account{ID: id, AccountType: "grok", GrokProvider: ProviderWeb, CredentialType: "sso", ClientCookie: "sso=tok", Enabled: true, Subscription: sub}
	acc.GrokWebQuota.SyncedAt = time.Now()
	if auto {
		acc.GrokWebQuota.Auto = store.GrokQuotaWindow{HasLimit: true, Limit: 50, HasRemaining: true, Remaining: 40}
	}
	if fast {
		acc.GrokWebQuota.Fast = store.GrokQuotaWindow{HasLimit: true, Limit: 140, HasRemaining: true, Remaining: 100}
	}
	return acc
}

func TestObservedRoutesFollowTheQuotaWindowsActuallyRead(t *testing.T) {
	testutil.Equal(t, len(observedRouteSet(nil)), 0)

	fastOnly := observedRouteSet([]*store.Account{webAccount(1, "basic", false, true)})
	_, chatFast := fastOnly["grok-chat-fast"]
	_, chatAuto := fastOnly["grok-chat-auto"]
	testutil.True(t, chatFast)
	testutil.False(t, chatAuto)

	super := observedRouteSet([]*store.Account{webAccount(1, "super", true, true)})
	_, heavy := super["grok-chat-heavy"]
	_, video := super["grok-imagine-video"]
	testutil.False(t, heavy)
	testutil.True(t, video)

	heavyAcc := observedRouteSet([]*store.Account{webAccount(1, "heavy", true, true)})
	_, heavy = heavyAcc["grok-chat-heavy"]
	testutil.True(t, heavy)

	stale := webAccount(1, "super", true, true)
	stale.GrokWebQuota.SyncedAt = time.Now().Add(-2 * webObservationTTL)
	disabled := webAccount(2, "super", true, true)
	disabled.Enabled = false
	rejected := webAccount(3, "super", true, true)
	rejected.AuthStatus = store.AccountAuthStatusReauthRequired
	testutil.Equal(t, len(observedRouteSet([]*store.Account{stale, disabled, rejected})), 0)
}

func TestPublishRoutesIsScopedToTheWebPlane(t *testing.T) {
	mini := miniredis.RunT(t)
	st, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "web-routes:"})
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	build := &store.Model{Channel: "grok", ModelID: "grok-4.5", Name: "Grok 4.5", Status: store.ModelStatusAvailable, Verified: true}
	store.ApplyGrokRouteDefaults(build)
	testutil.NoError(t, st.CreateModel(ctx, build))

	acc := webAccount(0, "super", true, true)
	testutil.NoError(t, st.CreateAccount(ctx, acc))
	testutil.NoError(t, PublishRoutes(ctx, st))

	row, err := st.GetModelByChannelAndModelID(ctx, "grok", "grok-chat-auto")
	testutil.NoError(t, err)
	testutil.Equal(t, row.Provider, ProviderWeb)
	testutil.True(t, row.Verified)
	testutil.True(t, row.SupportsCapability(store.CapabilityChat))
	testutil.True(t, row.SupportsCapability(store.CapabilityResponses))
	// The grok channel translates /messages to chat for Web conversations.
	testutil.True(t, row.SupportsCapability(store.CapabilityMessages))
	image, err := st.GetModelByChannelAndModelID(ctx, "grok", "grok-imagine-image")
	testutil.NoError(t, err)
	testutil.True(t, image.SupportsCapability(store.CapabilityImage))
	testutil.False(t, image.SupportsCapability(store.CapabilityImageEdit))
	edit, err := st.GetModelByChannelAndModelID(ctx, "grok", "grok-imagine-image-edit")
	testutil.NoError(t, err)
	testutil.True(t, edit.SupportsCapability(store.CapabilityImageEdit))

	// Without an observing account the Web plane publishes nothing; the Build
	// row is another plane's and survives.
	acc.Enabled = false
	testutil.NoError(t, st.UpdateAccount(ctx, acc))
	testutil.NoError(t, PublishRoutes(ctx, st))
	_, err = st.GetModelByChannelAndModelID(ctx, "grok", "grok-chat-auto")
	testutil.Error(t, err)
	kept, err := st.GetModelByChannelAndModelID(ctx, "grok", "grok-4.5")
	testutil.NoError(t, err)
	testutil.Equal(t, kept.Provider, "build")
}

func TestWebModelIdentifiersDoNotOverlapBuild(t *testing.T) {
	testutil.True(t, IsWebModelID("grok-chat-fast"))
	testutil.True(t, IsWebModelID("web/grok-imagine-image"))
	testutil.False(t, IsWebModelID("grok-4.5"))
	testutil.False(t, IsWebModelID("grok-composer-2.5-fast"))
}

func TestVerifyAccountFailsWhenTheQuotaReadFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/session" {
			_, _ = w.Write([]byte(`{"user":{"id":"u-1","email":"a@example.com"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<html><title>Just a moment...</title></html>`))
	}))
	defer upstream.Close()
	cfg := &config.Config{GrokWebBaseURL: upstream.URL}
	config.ApplyDefaults(cfg)
	acc := &store.Account{AccountType: "grok", GrokProvider: ProviderWeb, CredentialType: "sso", ClientCookie: "sso=tok-verify"}
	status, err := VerifyAccount(context.Background(), cfg, acc)
	testutil.Error(t, err)
	testutil.Equal(t, status, "")
	testutil.MustContain(t, acc.GrokWebSyncError, "Cloudflare")
	testutil.False(t, acc.GrokWebSyncAt.IsZero())
	testutil.Equal(t, len(observedRouteSet([]*store.Account{acc})), 0)
}

func TestGrokWebTransportOnlyReplacesTheProxy(t *testing.T) {
	cfg := &config.Config{ProxyURL: "http://general:1", ProxyBypass: []string{"x"}}
	testutil.Equal(t, cfg.GrokWebTransport(), cfg)
	cfg.GrokWebProxyURL = "socks5://web:2"
	web := cfg.GrokWebTransport()
	testutil.Equal(t, web.ProxyURL, "socks5://web:2")
	testutil.Equal(t, len(web.ProxyBypass), 0)
	testutil.Equal(t, cfg.ProxyURL, "http://general:1")
}
