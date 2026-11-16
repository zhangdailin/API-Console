package warp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"orchids-api/internal/middleware"
)

const (
	bindingTTL       = time.Hour
	bindingKeyPrefix = "warp:call:v2:"
	bindingLimit     = 4096
	maxToolRounds    = 6
)

// callBinding contains only continuation state, never account credentials or
// request headers. RootTaskContext is an official Request.TaskContext snapshot;
// its opaque history is private state and must never be logged or exposed.
type callBinding struct {
	ConversationID  string    `json:"conversation_id"`
	RequestID       string    `json:"request_id,omitempty"`
	RunID           string    `json:"run_id,omitempty"`
	OwnerScope      string    `json:"owner_scope"`
	AccountID       int64     `json:"account_id"`
	Model           string    `json:"model"`
	RootTaskContext []byte    `json:"root_task_context,omitempty"`
	ToolRounds      int       `json:"tool_rounds"`
	SavedAt         time.Time `json:"saved_at"`
}

// bindingStore keeps a bounded, expiring cache keyed by the complete ownership
// tuple. Production paths must use the explicit scoped methods.
type bindingStore struct {
	mu      sync.Mutex
	memory  map[string]callBinding
	claims  map[string]time.Time
	redis   redisBindingWriter
	keyBase string
}

type redisBindingWriter interface {
	SetBinding(ctx context.Context, key string, value []byte, ttl time.Duration) error
	GetBinding(ctx context.Context, key string) ([]byte, error)
}

func newBindingStore() *bindingStore {
	return &bindingStore{memory: make(map[string]callBinding)}
}

func bindingOwner(ctx context.Context) (string, bool) {
	if ctx == nil || middleware.APIKeyID(ctx) <= 0 {
		return "", false
	}
	// Both identities are non-secret. Including the fingerprint also separates
	// rotated keys that happen to retain the same database identity.
	return "key:" + strconv.FormatInt(middleware.APIKeyID(ctx), 10) + ":" + middleware.APIKeyFingerprint(ctx), true
}

func scopedBindingID(owner string, accountID int64, model, id string) string {
	// JSON is length-delimited and avoids ambiguous concatenated tuples.
	raw, _ := json.Marshal([]string{owner, strconv.FormatInt(accountID, 10), model, id})
	return hashID(string(raw))
}

// saveScoped refuses unauthenticated principals, oversized or invalid snapshots,
// and continuations beyond the fixed tool-turn budget. The input is copied.
func (b *bindingStore) saveScoped(ctx context.Context, accountID int64, model, id string, binding callBinding) bool {
	owner, ok := bindingOwner(ctx)
	if !ok || accountID <= 0 || model == "" {
		return false
	}
	return b.saveOwned(ctx, owner, accountID, model, id, binding)
}

func (b *bindingStore) saveOwned(ctx context.Context, owner string, accountID int64, model, id string, binding callBinding) bool {
	if b == nil || id == "" || binding.ConversationID == "" || binding.ToolRounds < 0 || binding.ToolRounds > maxToolRounds || len(binding.RootTaskContext) > continuationStateLimit {
		return false
	}
	if len(binding.RootTaskContext) > 0 {
		if _, err := decodeContinuationState(binding.RootTaskContext); err != nil {
			return false
		}
	}
	binding.OwnerScope, binding.AccountID, binding.Model = owner, accountID, model
	binding.SavedAt = time.Now().UTC()
	binding = cloneCallBinding(binding)
	id = scopedBindingID(owner, accountID, model, id)
	b.mu.Lock()
	previous, exists := b.memory[id]
	if exists && time.Since(previous.SavedAt) <= bindingTTL && !sameBindingResponse(previous, binding) {
		b.mu.Unlock()
		return false
	}
	writer, keyBase := b.redis, b.keyBase
	b.mu.Unlock()
	if writer != nil && owner != "local" {
		raw, err := json.Marshal(binding)
		if err != nil || writer.SetBinding(ctx, keyBase+bindingKeyPrefix+id, raw, bindingTTL) != nil {
			return false
		}
	}
	b.mu.Lock()
	b.cacheLocked(id, binding)
	b.mu.Unlock()
	return true
}

// lookupScoped resolves every ID in the exact principal/account/model scope.
// Misses, corrupt persisted records, expiry, or mixed snapshots fail closed.
func (b *bindingStore) lookupScoped(ctx context.Context, accountID int64, model string, ids []string) (callBinding, bool) {
	owner, ok := bindingOwner(ctx)
	if !ok || accountID <= 0 || model == "" {
		return callBinding{}, false
	}
	return b.lookupOwned(ctx, owner, accountID, model, ids)
}

func (b *bindingStore) lookupOwned(ctx context.Context, owner string, accountID int64, model string, ids []string) (callBinding, bool) {
	if b == nil || len(ids) == 0 {
		return callBinding{}, false
	}
	var found callBinding
	for i, callID := range ids {
		if callID == "" {
			return callBinding{}, false
		}
		id := scopedBindingID(owner, accountID, model, callID)
		b.mu.Lock()
		binding, ok := b.memory[id]
		writer, keyBase := b.redis, b.keyBase
		b.mu.Unlock()
		if !ok && writer != nil && owner != "local" {
			raw, err := writer.GetBinding(ctx, keyBase+bindingKeyPrefix+id)
			// JSON encodes the snapshot in base64; bound before allocating it.
			if err == nil && len(raw) > 0 && len(raw) <= 2*continuationStateLimit {
				ok = json.Unmarshal(raw, &binding) == nil
			}
		}
		age := time.Since(binding.SavedAt)
		if !ok || binding.OwnerScope != owner || binding.AccountID != accountID || binding.Model != model || binding.ConversationID == "" || age < 0 || age > bindingTTL || binding.ToolRounds < 0 || binding.ToolRounds > maxToolRounds || len(binding.RootTaskContext) > continuationStateLimit {
			return callBinding{}, false
		}
		if len(binding.RootTaskContext) > 0 {
			if _, err := decodeContinuationState(binding.RootTaskContext); err != nil {
				return callBinding{}, false
			}
		}
		if i == 0 {
			found = binding
		} else if binding.ConversationID != found.ConversationID || binding.RequestID != found.RequestID || binding.RunID != found.RunID || binding.ToolRounds != found.ToolRounds || !bytes.Equal(binding.RootTaskContext, found.RootTaskContext) {
			return callBinding{}, false
		}
	}
	return cloneCallBinding(found), true
}

func (b *bindingStore) cacheLocked(id string, binding callBinding) {
	if b.memory == nil {
		b.memory = make(map[string]callBinding)
	}
	if len(b.memory) >= bindingLimit {
		dropped := 0
		for key, value := range b.memory {
			if time.Since(value.SavedAt) > bindingTTL || dropped < bindingLimit/2 {
				delete(b.memory, key)
				dropped++
			}
		}
	}
	b.memory[id] = binding
}

// save and lookup preserve the historical test API. Keyless use is local to
// this constructor's memory and can never read or write shared Redis state.
func (b *bindingStore) save(ctx context.Context, id string, binding callBinding) {
	owner, ok := bindingOwner(ctx)
	if !ok {
		owner = "local"
	}
	b.saveOwned(ctx, owner, 0, "", id, binding)
}

func (b *bindingStore) lookup(ctx context.Context, ids []string) (callBinding, bool) {
	owner, ok := bindingOwner(ctx)
	if !ok {
		owner = "local"
	}
	return b.lookupOwned(ctx, owner, 0, "", ids)
}

func sameBindingResponse(a, b callBinding) bool {
	return a.OwnerScope == b.OwnerScope && a.AccountID == b.AccountID && a.Model == b.Model && a.ConversationID == b.ConversationID && a.RequestID == b.RequestID && a.RunID == b.RunID && a.ToolRounds == b.ToolRounds && bytes.Equal(a.RootTaskContext, b.RootTaskContext)
}

func cloneCallBinding(binding callBinding) callBinding {
	binding.RootTaskContext = bytes.Clone(binding.RootTaskContext)
	return binding
}

func hashID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}
