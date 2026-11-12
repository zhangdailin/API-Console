package grok

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/grok/web"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestStoreConvertedBuildAccountCreatesThenMergesByIdentity(t *testing.T) {
	mini := miniredis.RunT(t)
	st, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "sso-convert:"})
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	source := &store.Account{Name: "web-1", AccountType: "grok", GrokProvider: web.ProviderWeb, CredentialType: "sso", ClientCookie: "sso-cookie", Enabled: true}
	testutil.NoError(t, st.CreateAccount(ctx, source))

	// The access token is an opaque string here, so the identity comes from
	// the row itself: give both grants the same user through the first row.
	first, created, err := storeConvertedBuildAccount(ctx, st, source, deviceGrant{accessToken: "access-1", refreshToken: "refresh-1", expiresAt: time.Now().Add(time.Hour)})
	testutil.NoError(t, err)
	testutil.True(t, created)
	testutil.Equal(t, first.GrokProvider, ProviderBuild)
	testutil.Equal(t, first.CredentialType, "oauth")
	testutil.Equal(t, first.Name, "web-1 Build")

	// The Web source is untouched.
	kept, err := st.GetAccount(ctx, source.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, kept.ClientCookie, "sso-cookie")
	testutil.Equal(t, kept.GrokProvider, web.ProviderWeb)

	// Without a stable identity a second grant cannot be matched: it is a new row.
	second, created, err := storeConvertedBuildAccount(ctx, st, source, deviceGrant{accessToken: "access-2", refreshToken: "refresh-2", expiresAt: time.Now().Add(time.Hour)})
	testutil.NoError(t, err)
	testutil.True(t, created)
	testutil.True(t, second.ID != first.ID)
}

func TestSSOConvertFailureReasons(t *testing.T) {
	acc := &store.Account{ID: 1}
	reason, _ := ssoConvertFailure(web.ErrBrowserVerificationRequired, acc)
	testutil.Equal(t, reason, "browser_verification_required")
	reason, _ = ssoConvertFailure(web.ErrSSOSessionRejected, acc)
	testutil.Equal(t, reason, "sso_rejected")
	reason, _ = ssoConvertFailure(context.DeadlineExceeded, acc)
	testutil.Equal(t, reason, "timeout")
	reason, message := ssoConvertFailure(errors.New("grok device authorization failed: invalid_client"), acc)
	testutil.Equal(t, reason, "failed")
	testutil.MustContain(t, message, "invalid_client")
}
