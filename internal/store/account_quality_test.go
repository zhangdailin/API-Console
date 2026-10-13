package store

import (
	"context"
	"testing"
	"time"

	"orchids-api/internal/testutil"
)

func TestQualityVerdictSurvivesOrdinaryAccountRefresh(t *testing.T) {
	s, _ := newTestRedisStore(t, "quality-verdict:")
	ctx := context.Background()
	acc := &Account{AccountType: "grok", Enabled: true, Weight: 1, OAuthAccessToken: "access-token"}
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount: %v")
	stale, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount: %v")
	until := time.Now().UTC().Add(time.Hour)
	testutil.NoError(t, s.UpdateAccountQuality(ctx, acc.ID, 1, until, "no_reasoning"), "quality update: %v")
	stale.StatusCode = ""
	stale.StatusMessage = "ordinary refresh"
	testutil.NoError(t, s.UpdateAccount(ctx, stale), "ordinary update: %v")
	stored, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount: %v")
	testutil.Equal(t, stored.QualityFailures, 1)
	testutil.Equal(t, stored.QualityReason, "no_reasoning")
	testutil.True(t, stored.QualityCooldownUntil.Equal(until), "quality cooldown was erased")
	testutil.Equal(t, stored.StatusMessage, "")
	testutil.Equal(t, stored.OAuthAccessToken, "access-token")
	testutil.NoError(t, s.UpdateAccountQuality(ctx, acc.ID, 0, time.Time{}, "ignored stale reason"), "clear quality: %v")
	stored, err = s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount: %v")
	testutil.Equal(t, stored.QualityFailures, 0)
	testutil.Equal(t, stored.QualityReason, "")
	testutil.True(t, stored.QualityCooldownUntil.IsZero(), "healthy verdict should clear cooldown")
	testutil.True(t, stored.Enabled, "quality reset must not change operator enablement")
}
