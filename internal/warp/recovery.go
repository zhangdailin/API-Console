package warp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/upstream"
)

const recoveryTTL = time.Hour
const recoveryKeyPrefix = "warp:recovery:v1:"

var errRecoveryUnavailable = errors.New("Warp tool continuation recovery unavailable")

// recoveryHead contains only official private task state and scope metadata. No
// original request payload, credentials, or client-visible checkpoint is stored.
type recoveryHead struct {
	Version         int       `json:"version"`
	Revision        int       `json:"revision"`
	State           string    `json:"state"`
	Owner           string    `json:"owner"`
	AccountID       int64     `json:"account_id"`
	Model           string    `json:"model"`
	IDs             []string  `json:"ids"`
	Digest          string    `json:"digest"`
	ConversationID  string    `json:"conversation_id"`
	RequestID       string    `json:"request_id,omitempty"`
	RootTaskContext []byte    `json:"root_task_context,omitempty"`
	PaidTurns       int       `json:"paid_turns"`
	Attempts        int       `json:"attempts"`
	SavedAt         time.Time `json:"saved_at"`
}

type recoveryStore struct {
	mu     sync.Mutex
	memory map[string][]byte
	// These counters are independent of a head and survive successive tool turns.
	attempts map[string]int
	paid     map[string]int
	expires  map[string]time.Time
}

func newRecoveryStore() *recoveryStore {
	return &recoveryStore{memory: map[string][]byte{}, attempts: map[string]int{}, paid: map[string]int{}, expires: map[string]time.Time{}}
}

// recoveryDigest binds normalized instructions, ordered tools, original dialog,
// and results. RequestID and cache hints are not authorization or resume tokens.
func recoveryDigest(req upstream.UpstreamRequest, prepared preparedRequest) (string, error) {
	system, messages, err := normalizeInstructions(req.System, req.Messages)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		System      string
		Messages    any
		Tools       []mcpTool
		Parallel    bool
		Results     []preparedToolResult
		CurrentText string
	}{system, messages, prepared.Tools, prepared.ParallelTools, prepared.ToolResults, prepared.CurrentText})
	if err != nil {
		return "", err
	}
	return hashID(string(raw)), nil
}

func recoveryIdentity(ctx context.Context, accountID int64, model string, ids []string) (recoveryHead, string, bool) {
	owner, ok := bindingOwner(ctx)
	if !ok || accountID <= 0 || model == "" || len(ids) == 0 {
		return recoveryHead{}, "", false
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i, id := range sorted {
		if id == "" || (i > 0 && sorted[i-1] == id) {
			return recoveryHead{}, "", false
		}
	}
	raw, _ := json.Marshal([]any{owner, accountID, model, sorted})
	return recoveryHead{Version: 1, Owner: owner, AccountID: accountID, Model: model, IDs: sorted}, hashID(string(raw)), true
}

func recoveryCounterKeys(b *bindingStore, head recoveryHead) (string, string) {
	base := head.Owner + "\x00" + strconv.FormatInt(head.AccountID, 10) + "\x00" + head.Model
	scope := hashID(base + "\x00" + head.ConversationID)
	return b.keyBase + "warp:recovery-attempts:v1:" + scope, b.keyBase + "warp:turn:" + scope
}

func validRecoveryHead(head recoveryHead, identity recoveryHead, digest string) bool {
	age := time.Since(head.SavedAt)
	if head.Version != 1 || head.Revision < 1 || head.State != "failed" || head.Owner != identity.Owner || head.AccountID != identity.AccountID || head.Model != identity.Model || head.Digest != digest || head.ConversationID == "" || head.Attempts < 0 || head.PaidTurns < 1 || age < 0 || age > recoveryTTL || len(head.IDs) != len(identity.IDs) {
		return false
	}
	for i := range head.IDs {
		if head.IDs[i] != identity.IDs[i] {
			return false
		}
	}
	_, err := decodeContinuationState(head.RootTaskContext)
	return err == nil
}

func (c *Client) recoveryBackend() (*recoveryStore, *bindingStore) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.recovery == nil {
		c.recovery = newRecoveryStore()
	}
	return c.recovery, c.bindings
}

func (c *Client) loadRecovery(ctx context.Context, accountID int64, model string, ids []string, digest string) (recoveryHead, []byte, error) {
	identity, key, ok := recoveryIdentity(ctx, accountID, model, ids)
	if !ok {
		return recoveryHead{}, nil, errRecoveryUnavailable
	}
	local, bindings := c.recoveryBackend()
	if bindings == nil {
		return recoveryHead{}, nil, errRecoveryUnavailable
	}
	raw, err := recoveryRead(ctx, bindings, local, bindings.keyBase+recoveryKeyPrefix+key)
	if err != nil || len(raw) == 0 || len(raw) > 2*continuationStateLimit {
		return recoveryHead{}, nil, errRecoveryUnavailable
	}
	var head recoveryHead
	if json.Unmarshal(raw, &head) != nil || !validRecoveryHead(head, identity, digest) {
		return recoveryHead{}, nil, errRecoveryUnavailable
	}
	return head, raw, nil
}

// reserveRecovery is the sole transition that permits another paid generation.
// The checkpoint CAS and observational counters are atomic across processes.
func (c *Client) reserveRecovery(ctx context.Context, head recoveryHead, previous []byte) (recoveryHead, []byte, error) {
	identity, key, ok := recoveryIdentity(ctx, head.AccountID, head.Model, head.IDs)
	if !ok || !validRecoveryHead(head, identity, head.Digest) {
		return recoveryHead{}, nil, errRecoveryUnavailable
	}
	next := head
	next.Revision++
	next.State = "running"
	next.Attempts++
	next.PaidTurns++
	raw, err := json.Marshal(next)
	if err != nil {
		return recoveryHead{}, nil, errRecoveryUnavailable
	}
	local, bindings := c.recoveryBackend()
	attempts, paid := recoveryCounterKeys(bindings, head)
	if err := recoveryCAS(ctx, bindings, local, bindings.keyBase+recoveryKeyPrefix+key, previous, raw, attempts, paid, head); err != nil {
		return recoveryHead{}, nil, err
	}
	return next, raw, nil
}

func recoverySnapshotMatches(initial, current []byte) bool {
	before, err := decodeContinuationState(initial)
	if err != nil {
		return false
	}
	after, err := decodeContinuationState(current)
	return err == nil && before.GetId() == after.GetId()
}

// finishRecovery creates a failed head only from a verified parser checkpoint.
// A running head is always invalidated on success or any non-recoverable error.
func (c *Client) finishRecovery(ctx context.Context, accountID int64, model string, ids []string, digest string, original callBinding, reserved *recoveryHead, previous []byte, result streamResult, failed bool, requestID string) bool {
	identity, key, ok := recoveryIdentity(ctx, accountID, model, ids)
	if !ok {
		return false
	}
	safe := failed && result.RecoverySafe && result.ConversationID == original.ConversationID && recoverySnapshotMatches(original.RootTaskContext, result.RecoveryContext)
	if reserved == nil && !safe {
		return false
	}
	head := identity
	if reserved != nil {
		head = *reserved
	} else {
		head.Revision, head.Digest, head.ConversationID = 1, digest, original.ConversationID
		head.RequestID, head.PaidTurns = requestID, original.ToolRounds+1
	}
	head.State = "done"
	head.RootTaskContext = nil
	if safe {
		head.State = "failed"
		head.RootTaskContext = bytes.Clone(result.RecoveryContext)
	}
	head.Revision++
	head.SavedAt = time.Now().UTC()
	raw, err := json.Marshal(head)
	if err != nil {
		return false
	}
	local, bindings := c.recoveryBackend()
	if bindings == nil {
		return false
	}
	// Detached bounded cleanup retains authenticated scope even on cancellation.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if recoveryCAS(cleanup, bindings, local, bindings.keyBase+recoveryKeyPrefix+key, previous, raw, "", "", head) != nil {
		return false
	}
	return head.State == "failed"
}

func explicitRecoveryError() error {
	return apperrors.New("warp_continuation_conflict", "Warp tool continuation recovery is unavailable, already running, or completed; no upstream call was made", 409)
}

func recoveryAvailableError() error {
	return apperrors.New("warp_generation_failed", "Warp tool continuation failed; explicit resume is available with X-Warp-Resume: true and the same original request; no automatic retry", 502).WithDetails(map[string]any{"explicit_resume_available": true})
}
