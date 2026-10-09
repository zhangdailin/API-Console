package middleware

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/channel"
	"orchids-api/internal/pricing"
)

// DefaultBillingReservationTTL bounds how long a pre-flight hold can outlive a
// request that never settled it. A crashed replica therefore leaks capacity for
// at most this long.
const DefaultBillingReservationTTL = 30 * time.Minute

// billingReleaseTimeout bounds the release issued from a request's deferred
// cleanup, which must still happen when the client already disconnected.
const billingReleaseTimeout = 5 * time.Second

// maxBillingBodyBytes bounds the request body buffered for cost estimation.
// Bodies beyond it are still delivered to the handler in full; only the
// reservation is computed from the prefix that was read.
const maxBillingBodyBytes = 8 << 20

// BillingReservation is the hold one request took against its client key. The
// request-scoped pointer is mutated by SettleAPIKeyBilling so the deferred
// cleanup can tell whether a charge already happened.
type BillingReservation struct {
	KeyID   int64
	EventID string
	Amount  int64

	mu      sync.Mutex
	settled bool
	priced  pricing.Result
}

// Settled reports whether this reservation has been booked as usage. It is what
// keeps a settled request from also being released, and a released one from
// being charged twice.
func (r *BillingReservation) Settled() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settled
}

// claim marks the reservation settled exactly once. The second caller gets the
// first caller's pricing result and the false return, which is what makes
// settlement idempotent per event id.
func (r *BillingReservation) claim(result pricing.Result) (pricing.Result, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settled {
		return r.priced, false
	}
	r.settled = true
	r.priced = result
	return result, true
}

type apiKeyBillingReservationContextKey struct{}

// WithBillingReservation attaches a hold to the request context so the handler
// that finishes the request can settle it.
func WithBillingReservation(ctx context.Context, reservation *BillingReservation) context.Context {
	if ctx == nil || reservation == nil {
		return ctx
	}
	return context.WithValue(ctx, apiKeyBillingReservationContextKey{}, reservation)
}

// BillingReservationFrom returns the hold taken for this request, or nil when
// the client key has no billing limit (or this route is not billed).
func BillingReservationFrom(ctx context.Context) *BillingReservation {
	if ctx == nil {
		return nil
	}
	reservation, _ := ctx.Value(apiKeyBillingReservationContextKey{}).(*BillingReservation)
	return reservation
}

// APIKeyBillingStore is the settlement half of the client-key billing ledger.
// It is deliberately tiny so the middleware does not depend on the concrete
// store: any implementation of these two methods can be settled against.
type APIKeyBillingStore interface {
	SettleApiKeyBilling(ctx context.Context, id int64, eventID string, amount int64) error
	ReleaseApiKeyBilling(ctx context.Context, id int64, eventID string) (bool, error)
}

// APIKeyBillingReserver is the reservation half, used by
// APIKeyBillingReservation before a request is handed to its handler.
type APIKeyBillingReserver interface {
	ReserveApiKeyBilling(ctx context.Context, id int64, eventID string, amount int64, expiresAt time.Time) (bool, error)
	ReleaseApiKeyBilling(ctx context.Context, id int64, eventID string) (bool, error)
}

// apiKeyBillingStore is the process-wide ledger used when a settle call site
// passes nil. Wiring it once at startup keeps every channel's settle path
// identical instead of threading the store through each handler.
var apiKeyBillingStore APIKeyBillingStore

// SetAPIKeyBillingStore wires the default billing ledger for settle call sites
// that do not carry one. Passing nil disables it.
func SetAPIKeyBillingStore(store APIKeyBillingStore) { apiKeyBillingStore = store }

type settleOptions struct {
	allowEstimates bool
}

// SettleOption customises one settlement. Options are explicit so the default
// can stay conservative: only upstream-reported usage is billable.
type SettleOption func(*settleOptions)

// SettleAPIKeyBilling computes the official cost of one finished request and
// books it against the reservation taken before the request ran, returning the
// pricing result for the audit row.
//
// Rules:
//   - Only upstream-reported usage is billable. An estimated row returns false
//     and is never charged (A9-2 semantics: an estimate must not become an
//     invoice).
//   - An unpriced model returns false, which stays distinguishable from a
//     genuine zero cost.
//   - Settlement is idempotent per event id: a repeated call returns the first
//     result and charges nothing further.
//   - A request whose key has no limit was never reserved; its cost is still
//     returned so the audit row can carry it.
func SettleAPIKeyBilling(
	ctx context.Context,
	settler APIKeyBillingStore,
	model string,
	usageSource audit.UsageSource,
	inputTokens, cachedInputTokens, outputTokens int64,
	options ...SettleOption,
) (pricing.Result, bool) {
	settings := settleOptions{}
	for _, option := range options {
		if option != nil {
			option(&settings)
		}
	}
	if usageSource != audit.UsageSourceUpstream && !settings.allowEstimates {
		return pricing.Result{}, false
	}
	result, priced := pricing.EstimateCost(model, inputTokens, cachedInputTokens, outputTokens, inputTokens)
	if !priced {
		return pricing.Result{}, false
	}
	if settler == nil {
		settler = apiKeyBillingStore
	}
	reservation := BillingReservationFrom(ctx)
	if settler == nil || reservation == nil || reservation.KeyID == 0 || reservation.EventID == "" {
		// Nothing was held for this request: the cost belongs on the audit row,
		// but there is no capacity to convert into usage.
		return result, true
	}
	claimed, first := reservation.claim(result)
	if !first {
		return claimed, true
	}
	if err := settler.SettleApiKeyBilling(ctx, reservation.KeyID, reservation.EventID, result.CostInUSDTicks); err != nil {
		// The hold is intentionally left claimed: retrying a settlement that may
		// have reached Redis would charge the key twice. The hold expires with its
		// TTL instead, and the audit row still records the cost.
		slog.Warn("failed to settle API key billing",
			"error", err, "key_id", reservation.KeyID, "event_id", reservation.EventID,
			"amount_ticks", result.CostInUSDTicks)
	}
	return result, true
}

// APIKeyBillingReservation reserves the worst-case text cost of one inference
// request against the client key's limit before the handler runs.
//
// It must sit inside APIKeyAuth (the principal carries the limit) and read the
// body only for JSON inference requests, which it restores untouched. A refused
// reservation answers 402 and never reaches the handler; a reservation that
// nothing settled is released when the request ends.
func APIKeyBillingReservation(next http.HandlerFunc, reserver APIKeyBillingReserver, ttl time.Duration) http.HandlerFunc {
	if ttl <= 0 {
		ttl = DefaultBillingReservationTTL
	}
	return func(w http.ResponseWriter, r *http.Request) {
		principal, _ := r.Context().Value(apiKeyPrincipalContextKey{}).(*APIKeyPrincipal)
		if principal == nil || principal.ID == 0 || principal.BillingLimitUSDTicks <= 0 || reserver == nil {
			next(w, r)
			return
		}
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			next(w, r)
			return
		}
		if !billingRequestPath(r.URL.Path) {
			next(w, r)
			return
		}
		body := readBillingBody(r)
		if len(body) == 0 {
			next(w, r)
			return
		}
		reservation, priced := pricing.EstimateTextReservationFromBody(body)
		if !priced {
			// An unpriced model cannot be estimated, and guessing a rate would be
			// worse than not holding a budget for it.
			next(w, r)
			return
		}
		// A hold without a stable identity could never be settled or released
		// again, so an unmetered request is preferable to a leaked reservation.
		eventID := GetRequestID(r.Context())
		if eventID == "" {
			next(w, r)
			return
		}
		allowed, err := reserver.ReserveApiKeyBilling(r.Context(), principal.ID, eventID, reservation.CostInUSDTicks, time.Now().UTC().Add(ttl))
		if err != nil {
			// Failing closed matches APIKeyAuth: a key that asked for a budget is
			// never allowed to spend blindly when the ledger cannot be consulted.
			slog.Error("failed to reserve API key billing", "error", err, "key_id", principal.ID, "event_id", eventID)
			writeAPIKeyError(w, http.StatusServiceUnavailable, "API key billing check unavailable", "billing_unavailable")
			return
		}
		if !allowed {
			writeAPIKeyError(w, http.StatusPaymentRequired, "API key billing limit exceeded", "billing_limit_exceeded")
			return
		}
		held := &BillingReservation{KeyID: principal.ID, EventID: eventID, Amount: reservation.CostInUSDTicks}
		ctx := WithBillingReservation(r.Context(), held)
		defer func() {
			if held.Settled() {
				return
			}
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), billingReleaseTimeout)
			defer cancel()
			if _, err := reserver.ReleaseApiKeyBilling(releaseCtx, held.KeyID, held.EventID); err != nil {
				slog.Warn("failed to release API key billing reservation",
					"error", err, "key_id", held.KeyID, "event_id", held.EventID)
			}
		}()
		next(w, r.WithContext(ctx))
	}
}

// billingRequestPath reports whether this path is an inference endpoint whose
// body carries a model and a prompt.
// billingRequestPath selects the inference paths whose usage can be settled.
//
// /messages/count_tokens shares the /messages prefix but produces no completion
// and therefore never settles: holding a reservation for it would refuse a
// counting request from a key that is already at its ceiling without ever
// charging for it.
func billingRequestPath(path string) bool {
	_, endpoint, ok := channel.EndpointFromPath(path)
	if !ok {
		return false
	}
	switch strings.TrimRight(endpoint, "/") {
	case "messages", "chat/completions", "responses", "responses/compact":
		return true
	}
	return false
}

// readBillingBody buffers the request body for estimation and restores it for
// the handler. A body larger than the cap is re-served in full through a
// MultiReader and estimated from the prefix that was read.
func readBillingBody(r *http.Request) []byte {
	if r == nil || r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBillingBodyBytes+1))
	if err != nil {
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
		return nil
	}
	if len(raw) > maxBillingBodyBytes {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), r.Body))
		return raw[:maxBillingBodyBytes]
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	return raw
}
