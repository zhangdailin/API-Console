package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"orchids-api/internal/middleware"
	"orchids-api/internal/prompt"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

const requestAffinityTTL = time.Hour

type requestAffinity struct {
	Provider, Model, Key, SessionID string
}

func prepareRequestAffinity(r *http.Request, req ClaudeRequest, channel string) requestAffinity {
	spec, ok := provider.Lookup(channel)
	if !ok || !spec.ConversationAffinity {
		return requestAffinity{}
	}
	return deriveRequestAffinity(middleware.APIKeyFingerprint(r.Context()), r, req, channel)
}

// Explicit identities may resume an upstream session. A prefix is only an
// account preference: unrelated conversations can start with identical text.
func deriveRequestAffinity(owner string, r *http.Request, req ClaudeRequest, channel string) requestAffinity {
	if owner == "" || strings.TrimSpace(req.Model) == "" {
		return requestAffinity{}
	}
	id := explicitConversationID(r, req)
	if id == "" {
		id = metadataString(req.Metadata, "thread_id", "threadId", "session_id", "sessionId", "chat_id", "chatId")
	}
	if id == "" {
		id = headerValue(r, "X-Codex-Conversation-Id", "X-Codex-Session-Id", "X-Claude-Code-Session-Id", "X-Session-Id", "Session-Id", "X-Thread-Id", "X-Chat-Id")
	}
	if id == "" {
		id = metadataString(req.ClientMetadata, "conversation_id", "conversationId", "thread_id", "threadId", "session_id", "sessionId")
	}
	var seed any = []string{"explicit", id}
	if id == "" {
		var prefix []prompt.Message
		foundUser := false
		for _, message := range req.Messages {
			message.Content.Blocks = slices.Clone(message.Content.Blocks)
			for i := range message.Content.Blocks {
				message.Content.Blocks[i].CacheControl = nil
			}
			switch message.Role {
			case "system", "developer":
				prefix = append(prefix, message)
			case "user":
				prefix = append(prefix, message)
				foundUser = true
			}
			if foundUser {
				break
			}
		}
		if !foundUser {
			return requestAffinity{}
		}
		system := slices.Clone(req.System)
		for i := range system {
			system[i].CacheControl = nil
		}
		seed = []any{"prefix", system, prefix}
	}
	affinity := requestAffinity{Provider: strings.ToLower(strings.TrimSpace(channel)), Model: strings.ToLower(strings.TrimSpace(req.Model))}
	hash := sha256.New()
	if json.NewEncoder(hash).Encode([]any{owner, affinity.Provider, affinity.Model, seed}) != nil {
		return requestAffinity{}
	}
	affinity.Key = hex.EncodeToString(hash.Sum(nil))
	if id != "" {
		affinity.SessionID = "chat_" + affinity.Key
	}
	return affinity
}

func (h *Handler) preferredAccount(ctx context.Context, affinity requestAffinity) int64 {
	if affinity.Key == "" || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	entry, err := h.loadBalancer.Store.GetSessionAffinity(ctx, affinity.Provider, affinity.Model, affinity.Key)
	if err != nil {
		slog.Warn("Conversation affinity lookup failed", "provider", affinity.Provider)
		return 0
	}
	if entry != nil && entry.Provider == affinity.Provider && entry.Model == affinity.Model && entry.SessionKey == affinity.Key && time.Now().Before(entry.ExpiresAt) {
		return entry.AccountID
	}
	return 0
}

func (h *Handler) rememberAccount(ctx context.Context, affinity requestAffinity, account *store.Account) {
	if affinity.Key == "" || account == nil || account.ID == 0 || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if err := h.loadBalancer.Store.SaveSessionAffinity(ctx, &store.StoredSessionAffinity{
		Provider: affinity.Provider, Model: affinity.Model, SessionKey: affinity.Key, AccountID: account.ID,
	}, requestAffinityTTL); err != nil {
		slog.Warn("Conversation affinity save failed", "provider", affinity.Provider, "account_id", account.ID)
	}
}
