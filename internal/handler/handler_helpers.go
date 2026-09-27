package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/cline"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
	"orchids-api/internal/workbuddy"
)

func normalizeRequestedModelID(modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	return modelID
}

func (h *Handler) resolveModelAlias(ctx context.Context, modelID string) (string, *store.Model) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return modelID, nil
	}
	candidate := normalizeRequestedModelID(modelID)
	if candidate == "" {
		return modelID, nil
	}
	if m, err := h.loadBalancer.Store.GetModelByModelID(ctx, candidate); err == nil && m != nil {
		return candidate, m
	}
	return modelID, nil
}

func (h *Handler) resolveModelAliasForChannel(ctx context.Context, channel, modelID string) (string, *store.Model) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return modelID, nil
	}
	candidate := normalizeRequestedModelID(modelID)
	if candidate == "" {
		return modelID, nil
	}
	if m, err := h.loadBalancer.Store.GetModelByChannelAndModelID(ctx, channel, candidate); err == nil && m != nil {
		return candidate, m
	}
	return modelID, nil
}

// ChannelForModel reports the channel a model id is registered under, or an
// empty string when the model is unknown. The unified /v1 routes use it to pick
// between the native and the bridged implementation.
//
// The request model hint is consulted first: the unified entry points publish
// the exact model string they are about to serve, and a Codex client asks about
// the *family* name ("gpt-5-6-sol") while the catalog only stores the
// effort-suffixed variants. Resolving through the hint keeps the answer
// identical to the one the request path will compute, instead of a second,
// subtly different lookup.
func (h *Handler) ChannelForModel(ctx context.Context, modelID string) string {
	channel, _ := h.LookupChannelForModel(ctx, modelID)
	return channel
}

// LookupChannelForModel is ChannelForModel with the store error preserved. A
// caller that must choose between two implementations needs to tell "this model
// belongs to another channel" from "the store could not be read"; collapsing
// both onto an empty channel is how a Redis hiccup turns into a wrong route.
func (h *Handler) LookupChannelForModel(ctx context.Context, modelID string) (string, error) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return "", nil
	}
	if hinted := strings.TrimSpace(middleware.RequestModelFromContext(ctx)); hinted != "" {
		modelID = hinted
	}
	modelID = normalizeRequestedModelID(modelID)
	if modelID == "" {
		return "", nil
	}
	m, err := h.loadBalancer.Store.GetModelByModelID(ctx, modelID)
	if err == nil && m != nil {
		return strings.TrimSpace(m.Channel), nil
	}
	if err != nil && !isModelMissingError(err) {
		return "", err
	}
	// A family name is not a catalog row. Fall back to the effort variant the
	// request path would have selected, so "/v1/models/gpt-5-6-sol" and
	// "/v1/chat/completions" agree on the channel.
	variant := h.resolveEffortModelVariant(ctx, modelID, "", "")
	if variant == "" || variant == modelID {
		return "", nil
	}
	m, err = h.loadBalancer.Store.GetModelByModelID(ctx, variant)
	if err != nil {
		if isModelMissingError(err) {
			return "", nil
		}
		return "", err
	}
	if m == nil {
		return "", nil
	}
	return strings.TrimSpace(m.Channel), nil
}

// isModelMissingError reports whether a model lookup failed because the row does
// not exist. The stores signal that both ways — ErrNoRows and a plain
// "model not found" — and a caller that separates "unknown model" from "store
// unavailable" has to accept both, or every miss looks like an outage.
func isModelMissingError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrNoRows) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "model not found")
}

// ModelChannel reports the channel a request should be served by. It is the one
// place that decides between "the path already names a channel" and "the model
// decides". Every entry point that used to call channelFromPath on a body that
// carries a model must call this instead: on the unified prefix the path carries
// no channel at all, so a path-only answer silently degrades to the generic
// code path and the token count comes back with the wrong profile).
func (h *Handler) ModelChannel(r *http.Request, modelID string) string {
	if r != nil {
		if channel := channelFromPath(r.URL.Path); channel != "" {
			return channel
		}
	}
	if h == nil {
		return ""
	}
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	if modelID == "" {
		if r != nil {
			modelID = middleware.RequestModelFromContext(ctx)
		}
	}
	return h.ChannelForModel(ctx, modelID)
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
	lookup := func(id string) *store.Model {
		if forcedChannel != "" {
			_, m := h.resolveModelAliasForChannel(ctx, forcedChannel, id)
			return m
		}
		_, m := h.resolveModelAlias(ctx, id)
		return m
	}
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
// package remain account-wide parked.
func honorsModelCooldown(channel string) bool {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "qoder", "workbuddy":
		return true
	default:
		return false
	}
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
	needsFilter := model != "" && (honorsModelCooldown(channel) || channel == "qoder" || channel == "workbuddy" || channel == "cline")
	if needsFilter {
		return h.loadBalancer.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, failedAccountIDs, targetChannel, h.connTracker, func(acc *store.Account) bool {
			if channel == "cline" && !cline.CatalogSupportsModel(acc.ClineModelIDs, model) {
				return false
			}
			if honorsModelCooldown(channel) && store.ModelCooldownRemaining(acc, model, time.Now()) != 0 {
				return false
			}
			switch strings.TrimSpace(acc.StatusCode) {
			case "402":
				if channel == "qoder" {
					return qoder.IsFreeModel(acc.QoderModelIDs, model) && h.isCurrentFreeModel(ctx, "qoder", model)
				}
				if channel == "workbuddy" {
					return workbuddy.IsFreeModelInCatalog(acc.WorkBuddyModelIDs, model)
				}
			case store.AccountStatusQoderQuotaExhausted:
				return channel == "qoder" && qoder.IsFreeModel(acc.QoderModelIDs, model) && h.isCurrentFreeModel(ctx, "qoder", model)
			case store.AccountStatusWorkBuddyQuotaExhausted:
				return channel == "workbuddy" && workbuddy.IsFreeModelInCatalog(acc.WorkBuddyModelIDs, model)
			default:
				return true
			}
			return false
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
	var m *store.Model
	if forcedChannel != "" {
		_, m = h.resolveModelAliasForChannel(ctx, forcedChannel, modelID)
	} else {
		_, m = h.resolveModelAlias(ctx, modelID)
	}
	if m == nil {
		return nil, fmt.Errorf("model not found")
	}
	if !m.Status.Enabled() {
		return nil, fmt.Errorf("model not available")
	}
	if forcedChannel != "" {
		mChannel := strings.TrimSpace(m.Channel)
		if mChannel == "" {
			mChannel = ""
		}
		if !sameModelChannel(mChannel, forcedChannel) {
			return nil, fmt.Errorf("model not found")
		}
	}
	return m, nil
}

func sameModelChannel(a, b string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.ReplaceAll(value, "_", "-")
		value = strings.ReplaceAll(value, " ", "-")
		if value == "" {
			return ""
		}
		return value
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
			h.statsMu.Lock()
			var pendingKey string
			var delta accountStatsDelta
			found := false
			for key, pending := range h.statsPending {
				pendingKey, delta, found = key, pending, true
				delete(h.statsPending, key)
				break
			}
			h.statsMu.Unlock()
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
		h.statsMu.Lock()
		var pendingKey string
		var delta accountStatsDelta
		found := false
		for key, pending := range h.statsPending {
			pendingKey, delta, found = key, pending, true
			delete(h.statsPending, key)
			break
		}
		h.statsMu.Unlock()
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
	if errors.As(err, &hinted) {
		delay := hinted.RetryAfter()
		if delay > 30*time.Second {
			return 30 * time.Second
		}
		if delay > 0 {
			return delay
		}
	}
	return 0
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
func isSharedUpstreamRefusalClass(class apperrors.UpstreamErrorClass) bool {
	return class.Category == "rate_limit" && !class.SwitchAccount
}

// sharedRefusalWaitForChannel preserves Qoder's provider-normalized hint
// without shortening it further. Other channels retain their early-probe policy.
func sharedRefusalWaitForChannel(hint time.Duration, retry int, channel string) time.Duration {
	if strings.EqualFold(strings.TrimSpace(channel), "qoder") {
		if hint < 0 {
			return 0
		}
		return hint
	}
	return sharedRefusalWait(hint, retry)
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
