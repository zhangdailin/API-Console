package handler

import (
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

func TestDoubaoContinuationDoesNotMoveToAnotherAccount(t *testing.T) {
	s, _ := setupConnTrackerHandlerTest(t)
	first := createEnabledTestAccount(t, s, "first", "doubao")
	second := createEnabledTestAccount(t, s, "second", "doubao")
	ctx := middleware.WithAPIKeyFingerprint(t.Context(), "owner")
	if _, err := s.BindDoubaoConversation(ctx, "owner", "continued", first.ID); err != nil {
		t.Fatal(err)
	}
	lb := loadbalancer.NewWithCacheTTL(s, time.Nanosecond)
	h := NewWithLoadBalancer(&config.Config{}, lb)
	t.Cleanup(h.Close)
	h.SetClientFactory(func(*store.Account, *config.Config) UpstreamClient {
		return &trackerTestUpstream{}
	})
	_, selected, release, tracked, err := h.acquireInitialAccount(ctx, "doubao", "doubao", "continued")
	if err != nil || selected == nil || selected.ID != first.ID {
		t.Fatalf("unexpected account for pinned conversation: %v", err)
	}
	release()
	h.releaseTrackedAccount(tracked)
	first.Enabled = false
	if err := s.UpdateAccount(ctx, first); err != nil {
		t.Fatal(err)
	}

	_, selected, release, tracked, err = h.acquireInitialAccount(ctx, "doubao", "doubao", "continued")
	release()
	h.releaseTrackedAccount(tracked)
	if err == nil || selected != nil {
		t.Fatal("continued the conversation on an unrelated account")
	}

	otherContext := middleware.WithAPIKeyFingerprint(t.Context(), "other")
	_, selected, release, tracked, err = h.acquireInitialAccount(otherContext, "doubao", "doubao", "continued")
	defer release()
	defer h.releaseTrackedAccount(tracked)
	if err != nil || selected == nil || selected.ID != second.ID {
		t.Fatalf("another owner could not bind their own conversation: %v", err)
	}
	bound, err := s.DoubaoConversationAccount(otherContext, "other", "continued")
	if err != nil || bound != second.ID {
		t.Fatalf("incorrect new conversation binding: %v", err)
	}
}
