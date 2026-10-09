package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"orchids-api/internal/provider"
	"strings"
	"time"

	"encoding/json"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

func normalizeRequestedModelID(modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	return modelID
}

// lookupModelRow resolves a requested model id to its catalog row. The id is
// matched in normalized form; a non-empty channel limits the lookup to that
// channel's catalog. It returns nil when no row matches, the id is empty, or the
// store is unavailable.
func (h *Handler) lookupModelRow(ctx context.Context, channel, modelID string) *store.Model {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return nil
	}
	candidate := normalizeRequestedModelID(modelID)
	if candidate == "" {
		return nil
	}
	var (
		m   *store.Model
		err error
	)
	if channel == "" {
		m, err = h.loadBalancer.Store.GetModelByModelID(ctx, candidate)
	} else {
		m, err = h.loadBalancer.Store.GetModelByChannelAndModelID(ctx, channel, candidate)
	}
	if err != nil || m == nil {
		return nil
	}
	return m
}

// requestReasoningEffort returns the effort a client asked for, from whichever
// dialect it used: OpenAI's reasoning_effort or Anthropic's
// output_config.effort / thinking. A thinking budget without an explicit effort
// maps onto the same coarse levels so an effort-suffixed catalog can still be
// resolved.
func requestReasoningEffort(req ClaudeRequest) string {
	if effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort)); effort != "" {
		return effort
	}
	for _, config := range []map[string]interface{}{req.OutputConfig, req.Thinking} {
		value, ok := config["effort"].(string)
		if !ok {
			continue
		}
		if effort := strings.ToLower(strings.TrimSpace(value)); effort != "" {
			return effort
		}
	}
	if req.Thinking == nil {
		return ""
	}
	budget, ok := looseNumber(req.Thinking["budget_tokens"])
	if !ok || budget <= 0 {
		return ""
	}
	switch {
	case budget < 4096:
		return "low"
	case budget < 16384:
		return "medium"
	default:
		return "high"
	}
}

// looseNumber accepts the numeric shapes JSON decoding produces.
func looseNumber(value interface{}) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

// effortVariantOrder is the fallback order tried when a client asks for a model
// family by its bare name and the catalog only exposes effort-suffixed
// variants. A catalog may publish models as "<family>-<effort>" (gpt-5-6-sol-low),
// while clients such as Codex send the family name plus reasoning_effort.
var effortVariantOrder = []string{"medium", "high", "low", "xhigh", "max"}

// resolveEffortModelVariant maps a bare model name onto the catalog entry that
// actually exists. An exact catalog hit always wins; otherwise the requested
// reasoning_effort is tried as a suffix and then the default effort order.
// Without this, "gpt-5-6-sol" is rejected as "model not found" even though the
// family is available and the client stated which effort it wants.
//
// A model that already carries an effort suffix is never suffixed again: the
// old code would have tried "gpt-5-6-sol-low-low" and then fallen through to
// "-medium", silently serving an effort the client never asked for.
func (h *Handler) resolveEffortModelVariant(ctx context.Context, modelID, effort, forcedChannel string) string {
	modelID = normalizeRequestedModelID(modelID)
	if modelID == "" || h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return modelID
	}
	lookup := func(id string) *store.Model { return h.lookupModelRow(ctx, forcedChannel, id) }
	if m := lookup(modelID); m != nil {
		return modelID
	}
	// "gpt-5-6-sol-low" is final: it is an exact catalog entry under a name that
	// happens to end in an effort word, and appending another suffix can only
	// produce a wrong model.
	if _, level := splitEffortVariantSuffix(modelID); level != "" {
		return modelID
	}

	effort = strings.ToLower(strings.TrimSpace(effort))
	candidates := make([]string, 0, len(effortVariantOrder)+1)
	if effort != "" {
		candidates = append(candidates, modelID+"-"+effort)
	}
	for _, suffix := range effortVariantOrder {
		candidates = append(candidates, modelID+"-"+suffix)
	}
	for _, candidate := range candidates {
		if m := lookup(candidate); m != nil && m.Status.Enabled() {
			slog.Info("Resolved bare model to an effort variant",
				"model", modelID, "resolved", candidate, "reasoning_effort", effort, "channel", forcedChannel)
			return candidate
		}
	}
	return modelID
}

type accountSelectionOptions struct {
	ModelID string
}

// acquireAccountSelection is the form the request path uses: it returns the
// release handle for the account client, so a client evicted mid-request is
// closed only after that request finishes.
func (h *Handler) acquireAccountSelection(ctx context.Context, targetChannel string, channelRequired bool, failedAccountIDs []int64, opts accountSelectionOptions) (UpstreamClient, *store.Account, func(), error) {
	if h.loadBalancer != nil {
		if targetChannel != "" {
			slog.Debug("Account channel selection", "channel", targetChannel, "channel_required", channelRequired)
		}
		account, err := h.selectAccountRecordWithOptions(ctx, targetChannel, failedAccountIDs, opts)
		if err != nil {
			if channelRequired {
				return nil, nil, func() {}, err
			}
			if h.client != nil {
				slog.Debug("Load balancer: no available accounts for channel, using default config", "channel", targetChannel)
				return h.client, nil, func() {}, nil
			}
			return nil, nil, func() {}, err
		}
		client, release := h.acquireAccountClient(account)
		if client == nil {
			return nil, nil, func() {}, errors.New("no client configured")
		}
		return client, account, release, nil
	} else if h.client != nil {
		return h.client, nil, func() {}, nil
	}
	return nil, nil, func() {}, errors.New("no client configured")
}

// acquireReservedAccountSelection closes the check-then-increment race between
// account selection and connection tracking. A candidate is not returned until
// its per-account slot has been atomically reserved; if another request wins the
// last slot, selection continues with the remaining accounts.
func (h *Handler) acquireReservedAccountSelection(ctx context.Context, targetChannel string, channelRequired bool, failedAccountIDs []int64, opts accountSelectionOptions) (UpstreamClient, *store.Account, func(), int64, error) {
	excluded := append([]int64(nil), failedAccountIDs...)
	full := make(map[int64]struct{})
	// Account leases are held for the complete upstream request. When another
	// request is just finishing, an immediate second selection can observe all
	// accounts at their hard limit and turn a transient race into a 503. Give
	// releases a bounded, cancellable window to become visible before failing.
	// The two-second window matches the busy retry used by Qoder and is still
	// short enough that a genuinely saturated pool fails promptly.
	const (
		reservationRetries    = 8
		reservationRetryDelay = 250 * time.Millisecond
	)
	reservationAttempt := 0
	for {
		client, account, release, err := h.acquireAccountSelection(ctx, targetChannel, channelRequired, excluded, opts)
		if err != nil {
			if reservationAttempt < reservationRetries && strings.Contains(err.Error(), "all matching accounts are at their concurrency limit") {
				reservationAttempt++
				// Rebuild the exclusion set from caller-supplied failures. Entries in
				// full only represent accounts that lost a reservation race and may
				// be available again on the next pass.
				excluded = append([]int64(nil), failedAccountIDs...)
				clear(full)
				timer := time.NewTimer(reservationRetryDelay)
				select {
				case <-ctx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					return nil, nil, func() {}, 0, ctx.Err()
				case <-timer.C:
				}
				continue
			}
			if len(full) > 0 {
				return nil, nil, func() {}, 0, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are at their concurrency limit)", targetChannel)
			}
			return nil, nil, func() {}, 0, err
		}
		accountID, acquired := h.tryAcquireTrackedAccount(account)
		if acquired {
			return client, account, release, accountID, nil
		}
		release()
		if account == nil || account.ID == 0 {
			return nil, nil, func() {}, 0, fmt.Errorf("failed to reserve an account concurrency slot")
		}
		if _, seen := full[account.ID]; seen {
			return nil, nil, func() {}, 0, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are at their concurrency limit)", targetChannel)
		}
		full[account.ID] = struct{}{}
		excluded = append(excluded, account.ID)
	}
}

// honorsModelCooldown reports whether a channel's selection consults the
// per-model cooldown its own verdicts write. Qoder scopes agent/model windows;
// WorkBuddy may scope a plan refusal while accounts with a truly exhausted
// package remain account-wide parked. The answer now comes from the channel's
// registered capabilities instead of a hardcoded list here.
func honorsModelCooldown(channel string) bool {
	return provider.HonorsModelCooldown(channel)
}

func (h *Handler) isCurrentFreeModel(ctx context.Context, channel, modelID string) bool {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return false
	}
	model, err := h.loadBalancer.Store.GetModelByChannelAndModelID(ctx, channel, strings.ToLower(strings.TrimSpace(modelID)))
	return err == nil && model != nil && strings.EqualFold(strings.TrimSpace(model.BillingTier), "free")
}

func (h *Handler) selectAccountRecordWithOptions(ctx context.Context, targetChannel string, failedAccountIDs []int64, opts accountSelectionOptions) (*store.Account, error) {
	if h == nil || h.loadBalancer == nil {
		return nil, errors.New("load balancer not configured")
	}
	model := strings.TrimSpace(opts.ModelID)
	channel := strings.ToLower(strings.TrimSpace(targetChannel))
	// honorsModelCooldown already covers the qoder and workbuddy channels; cline
	// is the one extra channel whose catalog the filter below understands.
	needsFilter := model != "" && (honorsModelCooldown(channel) || channel == "cline")
	if needsFilter {
		return h.loadBalancer.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, failedAccountIDs, targetChannel, h.connTracker, func(acc *store.Account) error {
			if channel == "cline" && !provider.SupportsModel("cline", acc, model) {
				return loadbalancer.ErrAccountNotEligible
			}
			if honorsModelCooldown(channel) {
				// The verdict that recorded the cooldown travels with it, so an
				// emptied pool can say whether waiting could ever help.
				switch store.ModelCooldownKind(acc, model, time.Now()) {
				case store.ModelCooldownUnavailable:
					return loadbalancer.RejectModelUnavailable
				case store.ModelCooldownThrottled:
					return loadbalancer.RejectModelThrottled
				}
			}
			switch strings.TrimSpace(acc.StatusCode) {
			case "402":
				if channel == "qoder" {
					if provider.IsFreeModel("qoder", acc, model) && h.isCurrentFreeModel(ctx, "qoder", model) {
						return nil
					}
					return loadbalancer.ErrAccountNotEligible
				}
				if channel == "workbuddy" {
					if provider.IsFreeModel("workbuddy", acc, model) {
						return nil
					}
					return loadbalancer.ErrAccountNotEligible
				}
			case store.AccountStatusQoderQuotaExhausted:
				if channel == "qoder" && provider.IsFreeModel("qoder", acc, model) && h.isCurrentFreeModel(ctx, "qoder", model) {
					return nil
				}
				return loadbalancer.ErrAccountNotEligible
			case store.AccountStatusWorkBuddyQuotaExhausted:
				if channel == "workbuddy" && provider.IsFreeModel("workbuddy", acc, model) {
					return nil
				}
				return loadbalancer.ErrAccountNotEligible
			default:
				return nil
			}
			return loadbalancer.ErrAccountNotEligible
		})
	}
	return h.loadBalancer.GetNextAccountExcludingByChannelWithTracker(ctx, failedAccountIDs, targetChannel, h.connTracker)
}

func effectiveAccountConcurrencyLimit(acc *store.Account) int64 {
	return loadbalancer.EffectiveAccountConcurrencyLimit(acc)
}

func (h *Handler) tryAcquireTrackedAccount(acc *store.Account) (int64, bool) {
	if acc == nil || acc.ID == 0 {
		return 0, true
	}
	limit := effectiveAccountConcurrencyLimit(acc)
	if h != nil && h.connTracker != nil {
		if limited, ok := h.connTracker.(loadbalancer.LimitedConnTracker); ok && limit > 0 {
			if !limited.TryAcquire(acc.ID, limit) {
				return 0, false
			}
			return acc.ID, true
		}
		if limit > 0 && h.connTracker.GetCount(acc.ID) >= limit {
			return 0, false
		}
		h.connTracker.Acquire(acc.ID)
		return acc.ID, true
	}
	if h != nil && h.loadBalancer != nil {
		// The load balancer's built-in tracker is only a compatibility fallback;
		// production wires a shared tracker directly onto the handler.
		h.loadBalancer.AcquireConnection(acc.ID)
		return acc.ID, true
	}
	return 0, true
}

func (h *Handler) releaseTrackedAccount(accountID int64) {
	if accountID == 0 {
		return
	}
	if h != nil && h.connTracker != nil {
		h.connTracker.Release(accountID)
		return
	}
	if h != nil && h.loadBalancer != nil {
		h.loadBalancer.ReleaseConnection(accountID)
	}
}

func (h *Handler) validateModelAvailability(ctx context.Context, modelID, forcedChannel string) (*store.Model, error) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return nil, nil
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, nil
	}
	m := h.lookupModelRow(ctx, forcedChannel, modelID)
	if m == nil {
		return nil, fmt.Errorf("model not found")
	}
	if !m.Status.Enabled() {
		return nil, fmt.Errorf("model not available")
	}
	// A channel-pinned path must not serve a model that belongs to another
	// channel: it is reported as simply unknown for that route.
	if forcedChannel != "" && !sameModelChannel(m.Channel, forcedChannel) {
		return nil, fmt.Errorf("model not found")
	}
	return m, nil
}

func sameModelChannel(a, b string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.ReplaceAll(value, "_", "-")
		return strings.ReplaceAll(value, " ", "-")
	}
	return normalize(a) == normalize(b)
}

type accountStatsDelta struct {
	accountID   int64
	usage       float64
	count       int64
	operationID string
	completedAt time.Time
}

func (h *Handler) initAccountStatsWriter() {
	h.statsPending = make(map[string]accountStatsDelta)
	h.statsWake = make(chan struct{}, 1)
	h.statsStop = make(chan struct{})
	h.statsDone = make(chan struct{})
	go h.runAccountStatsWriter()
}

func (h *Handler) updateAccountStats(ctx context.Context, account *store.Account, inputTokens, outputTokens int) {
	if account == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return
	}
	h.statsOnce.Do(h.initAccountStatsWriter)
	completedAt := time.Now().UTC()
	requestID := middleware.GetRequestID(ctx)
	operationID := fmt.Sprintf("%d:%s", account.ID, requestID)
	if requestID == "" {
		operationID = fmt.Sprintf("stats-%d-%d", account.ID, completedAt.UnixNano())
	}
	delta := accountStatsDelta{accountID: account.ID, usage: float64(inputTokens + outputTokens), count: 1, operationID: operationID, completedAt: completedAt}
	h.statsMu.Lock()
	if h.statsClosed {
		h.statsMu.Unlock()
		return
	}
	// Keep each completion as its own durable operation. Coalescing by account
	// loses the request identity needed to make an ambiguous Redis retry safe.
	h.statsPending[operationID] = delta
	h.statsMu.Unlock()
	select {
	case h.statsWake <- struct{}{}:
	default:
	}
}

// popPendingStats takes one queued completion out of the pending map. It reports
// false when the queue is empty.
func (h *Handler) popPendingStats() (string, accountStatsDelta, bool) {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	for key, pending := range h.statsPending {
		delete(h.statsPending, key)
		return key, pending, true
	}
	return "", accountStatsDelta{}, false
}

func (h *Handler) runAccountStatsWriter() {
	defer close(h.statsDone)
	const (
		initialBackoff = 100 * time.Millisecond
		maxBackoff     = 5 * time.Second
	)
	backoff := initialBackoff
	for {
		select {
		case <-h.statsStop:
			return
		case <-h.statsWake:
		}
		for {
			pendingKey, delta, found := h.popPendingStats()
			if !found {
				backoff = initialBackoff
				break
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := h.loadBalancer.Store.IncrementAccountStatsOperation(ctx, delta.accountID, delta.usage, delta.count, delta.operationID, delta.completedAt)
			cancel()
			if err == nil {
				backoff = initialBackoff
				continue
			}

			// Requeue the identical operation. If Redis committed before the caller
			// observed an error, the durable operation id makes this retry a no-op.
			h.statsMu.Lock()
			h.statsPending[pendingKey] = delta
			h.statsMu.Unlock()
			slog.Error("Failed to update account stats; retrying", "account_id", delta.accountID, "operation_id", delta.operationID, "retry_in", backoff, "error", err)
			timer := time.NewTimer(backoff)
			select {
			case <-h.statsStop:
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}
	}
}

func (h *Handler) flushPendingAccountStats(timeout time.Duration) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pendingKey, delta, found := h.popPendingStats()
		if !found {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), remaining)
		err := h.loadBalancer.Store.IncrementAccountStatsOperation(ctx, delta.accountID, delta.usage, delta.count, delta.operationID, delta.completedAt)
		cancel()
		if err != nil {
			h.statsMu.Lock()
			h.statsPending[pendingKey] = delta
			h.statsMu.Unlock()
			slog.Warn("account stats remain unflushed during shutdown", "account_id", delta.accountID, "operation_id", delta.operationID, "error", err)
			return
		}
	}
}

type retryAfterError interface {
	RetryAfter() time.Duration
}

func upstreamRetryAfter(err error) time.Duration {
	var hinted retryAfterError
	if !errors.As(err, &hinted) {
		return 0
	}
	delay := hinted.RetryAfter()
	if delay <= 0 {
		return 0
	}
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func computeRetryDelay(base time.Duration, attempt int, category string) time.Duration {
	if base <= 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 4 {
		attempt = 4
	}
	delay := base * time.Duration(1<<(attempt-1))
	if category == "rate_limit" && delay < 2*time.Second {
		delay = 2 * time.Second
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay
}

func shouldRetryCurrentAccountWhenNoAlternative(category string) bool {
	switch strings.TrimSpace(category) {
	case "network", "timeout", "server", "model_unavailable", "unknown":
		return true
	default:
		return false
	}
}

// isSharedUpstreamRefusalClass reports whether this failure describes a resource
// shared by every account rather than this account's own limit. It is the
// category/switch pair the classifier produces for that shape, and it is also
// what tells the retry loop to keep the account it already holds: rotating
// would meet the identical refusal, so the wait is spent on the same one.
//
// upstream_unavailable (the upstream saying its own service is down for the
// model) belongs here beside the shared queue refusal: both are answerable only
// by waiting, neither is about the account, and both were previously reported to
// the client as this gateway's rate limit. upstream_queue is the same condition
// with the upstream naming a window, and is answered to the client as 429.
func isSharedUpstreamRefusalClass(class apperrors.UpstreamErrorClass) bool {
	if class.SwitchAccount {
		return false
	}
	return class.Category == "rate_limit" ||
		class.Category == "upstream_unavailable" ||
		class.Category == "upstream_queue"
}

// sharedRefusalTotalWaitBudget bounds how long one request may wait on a
// resource every account shares. The upstream's own hint is still honoured per
// attempt; this only stops a closed gate from holding a caller for minutes.
//
// The binding deadline is NOT the caller's patience, it is the shortest timeout
// in the chain in front of this process. Production answers behind Cloudflare,
// whose origin timeout is 100s: the first version of this bound was 90s and
// reasoned from the client's ~125s patience instead, so three of Qoder's 30s
// windows ran the request to 106s. Cloudflare gave up at 100s and the caller
// got a 520 "the origin web server sent a response Cloudflare could not parse"
// — an error no part of this gateway ever produced, for a request it was about
// to answer correctly at 106s.
//
// Measured on that deployment, one window plus its attempt costs ~33s and one
// attempt ~1.6s, so the wall clock is about 33*N + 1.6*(N+1):
//
//	N=1 ->  37s   the old behaviour, and what produced the 503s
//	N=2 ->  72s   the default: inside a 100s edge with ~28s to spare
//	N=3 -> 107s   past the edge, which is the 520 above
//
// So the default admits two windows, not three. The wait is charged against the
// budget as the upstream's own hint, without the jitter added to the sleep:
// jitter exists to decorrelate wake-ups, not to consume budget, and charging it
// is what made the previous 60s constant admit only one window in practice
// (30-35s booked twice straddled 60s and the second wait was refused whenever
// either jitter ran, which is almost always — the 503s at ~37s above).
//
// Two windows with honest accounting is strictly more waiting than the old
// constant delivered, and it stays inside the edge. The knob exists for a
// deployment whose edge tolerates more; see sharedRefusalEdgeSafeCeiling.
const sharedRefusalTotalWaitBudget = 60 * time.Second

// sharedRefusalEdgeSafeCeiling is the largest budget that is safe when the
// gateway is published through an edge proxy with a 100s origin timeout, which
// is Cloudflare's default and the shape this deployment runs in. Raising the
// budget past it does not buy a longer wait, it buys a 520: the edge hangs up
// first and the work the gateway did is discarded.
const sharedRefusalEdgeSafeCeiling = 60 * time.Second

// SharedRefusalWaitBudget is the effective wait bound. A configured
// shared_refusal_wait_budget_ms wins over the built-in default.
func SharedRefusalWaitBudget(configuredMs int) time.Duration {
	if configuredMs <= 0 {
		return sharedRefusalTotalWaitBudget
	}
	budget := time.Duration(configuredMs) * time.Millisecond
	if budget < time.Second {
		budget = time.Second
	}
	return budget
}

// SharedRefusalBudgetExceedsEdge reports whether a budget is larger than an edge
// proxy with a 100s origin timeout will allow to finish.
func SharedRefusalBudgetExceedsEdge(budget time.Duration) bool {
	return budget > sharedRefusalEdgeSafeCeiling
}

// WarnIfSharedRefusalBudgetExceedsEdge reports a configured budget that will be
// cut short by an edge proxy before the gateway can answer. It is silent for the
// default because the default is chosen to fit.
//
// This exists so the failure is visible in the log before it is visible to a
// caller as a 520. A silent over-long budget looks exactly like an upstream
// outage from the outside, which is how the mistake it guards against was found.
func WarnIfSharedRefusalBudgetExceedsEdge(budget time.Duration) {
	if !SharedRefusalBudgetExceedsEdge(budget) {
		return
	}
	slog.Warn("Shared-refusal wait budget exceeds the edge-safe ceiling; requests may be cut off by the edge proxy before they are answered",
		"budget", budget,
		"edge_safe_ceiling", sharedRefusalEdgeSafeCeiling,
		"note", "a 100s origin timeout (Cloudflare default) leaves no room for a budget this large plus the attempts and jitter around it")
}

// sharedRefusalWaitAllowedWithin reports whether one more wait of next fits
// inside the explicit budget after spending already on waits. It is a plain
// comparison so the bound can be tested without spending the waits themselves.
//
// next is the upstream's own hint, without the jitter that will be added to the
// actual sleep: jitter exists to decorrelate wake-ups, not to consume budget, so
// charging it here made the last reachable window unreachable.
func sharedRefusalWaitAllowedWithin(already, next, budget time.Duration) bool {
	if next <= 0 {
		return false
	}
	if budget <= 0 {
		budget = sharedRefusalTotalWaitBudget
	}
	return already+next <= budget
}

// sharedRefusalWaitForChannel uses an explicit Qoder retry interval when set;
// otherwise it preserves the provider hint. Other channels keep early probing.
func sharedRefusalWaitForChannel(hint time.Duration, retry int, channel string, qoderIntervalMs ...int) time.Duration {
	if strings.EqualFold(strings.TrimSpace(channel), "qoder") {
		if hint <= 0 {
			return 0
		}
		if len(qoderIntervalMs) > 0 && qoderIntervalMs[0] > 0 {
			ms := qoderIntervalMs[0]
			if ms > 86400000 {
				ms = 86400000
			}
			interval := time.Duration(ms) * time.Millisecond
			if interval < time.Second {
				interval = time.Second
			}
			return interval
		}
		return hint
	}
	return sharedRefusalWait(hint, retry)
}

func sharedRefusalSleepForChannel(wait time.Duration, channel string, qoderIntervalMs int) time.Duration {
	if qoderIntervalMs > 0 && strings.EqualFold(strings.TrimSpace(channel), "qoder") {
		return wait
	}
	return wait + sharedRefusalJitter(wait)
}

// sharedRefusalWait is the legacy early-probe policy for non-Qoder channels.
// It is not appropriate for Qoder's server-declared minimum RetryAfter.
func sharedRefusalWait(hint time.Duration, retry int) time.Duration {
	if hint <= 0 {
		return 0
	}
	fractions := [...]float64{1.0 / 16.0, 1.0 / 4.0, 3.0 / 4.0}
	fraction := 1.0
	if retry >= 1 && retry <= len(fractions) {
		fraction = fractions[retry-1]
	}
	wait := time.Duration(float64(hint) * fraction)
	if wait < time.Second {
		wait = time.Second
	}
	if wait > hint {
		wait = hint
	}
	return wait
}

// sharedRefusalJitter spreads retries that were all handed the same upstream
// recovery time, so they do not wake together and re-queue as one spike. It is
// bounded to a fifth of the wait (at most five seconds), which keeps the wait
// anchored to the upstream's own hint.
func sharedRefusalJitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return 0
	}
	jitter := delay / 5
	if jitter > 5*time.Second {
		jitter = 5 * time.Second
	}
	if jitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(jitter)))
}
