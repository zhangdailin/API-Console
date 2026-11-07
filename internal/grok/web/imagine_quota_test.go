package web

import (
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// Captured from production accounts (allowance data only).
const (
	paidImagineQuota = `{"image":{"available":true, "windowSizeSeconds":64800}, "imagePro":{"available":false, "remainingQueries":0, "windowSizeSeconds":64800}, "imageEdit":{"available":true, "remainingQueries":10, "windowSizeSeconds":64800}, "video":{"available":true, "remainingQueries":5, "windowSizeSeconds":64800}, "video720p":{"available":false, "remainingQueries":0, "windowSizeSeconds":64800}}`
	freeImagineQuota = `{"image":null, "imagePro":{"available":true, "remainingQueries":0, "windowSizeSeconds":86400, "nextAvailableAt":"2026-11-04T18:47:43.566564458Z"}, "imageEdit":null, "video":null, "video720p":{"available":true, "remainingQueries":1, "windowSizeSeconds":86400}}`
)

func TestImagineQuotaFencesMediaRouting(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	free, err := parseImagineQuota([]byte(freeImagineQuota), now)
	testutil.NoError(t, err)
	freeAcc := &store.Account{}
	freeAcc.GrokWebQuota.Imagine = free
	// Free plans offer no edits and no 480p video, have spent today's Imagine
	// images and keep their 720p video.
	testutil.False(t, mediaAllowed(freeAcc, mediaImageEdit, now))
	testutil.False(t, mediaAllowed(freeAcc, mediaVideo, now))
	testutil.False(t, mediaAllowed(freeAcc, mediaImagePro, now))
	testutil.True(t, mediaAllowed(freeAcc, mediaImagePro, now.Add(8*time.Hour)))
	testutil.True(t, mediaAllowed(freeAcc, mediaVideo720p, now))

	paid, err := parseImagineQuota([]byte(paidImagineQuota), now)
	testutil.NoError(t, err)
	paidAcc := &store.Account{}
	paidAcc.GrokWebQuota.Imagine = paid
	testutil.True(t, mediaAllowed(paidAcc, mediaImageEdit, now))
	testutil.True(t, mediaAllowed(paidAcc, mediaVideo, now))
	// A spent weekly pool refuses every media product, its own counters aside.
	paidAcc.GrokWebQuota.Weekly = store.GrokQuotaWindow{HasLimit: true, Limit: 100, HasRemaining: true, Remaining: 0, ResetAt: now.Add(48 * time.Hour)}
	testutil.False(t, mediaAllowed(paidAcc, mediaImageEdit, now))
	testutil.False(t, mediaAllowed(paidAcc, mediaImagePro, now))

	// An account never observed is allowed: its first request reveals it.
	testutil.True(t, mediaAllowed(&store.Account{}, mediaImageEdit, now))
}

func TestMediaProductForRoutes(t *testing.T) {
	video, _ := ResolveModel("grok-imagine-video")
	edit, _ := ResolveModel("grok-imagine-image-edit")
	image, _ := ResolveModel("grok-imagine-image")
	testutil.Equal(t, mediaProductFor(video, "720p"), mediaVideo720p)
	testutil.Equal(t, mediaProductFor(video, "480p"), mediaVideo)
	testutil.Equal(t, mediaProductFor(edit, ""), mediaImageEdit)
	testutil.Equal(t, mediaProductFor(image, ""), mediaImagePro)
	_, liteKnown := ResolveModel("grok-imagine-image-lite")
	testutil.False(t, liteKnown, "the lite image model is no longer served")
}
