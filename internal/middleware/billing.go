package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"orchids-api/internal/billingledger"
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

// BillingReservation is the hold one request took against its client key. The
// request-scoped pointer is mutated by SettleAPIKeyBilling so the deferred
// cleanup can tell whether a charge already happened.
type BillingReservation struct {
	KeyID   int64
	EventID string
	Amount  int64

	mu      sync.Mutex
	settled bool
	pending bool
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

// Pending includes ambiguous writes. Cleanup must not release their holds.
func (r *BillingReservation) Pending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending
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
// pricing result for the audit row. The boolean is false for an unpriced row
// or a settlement still pending; a nonempty Result.Model preserves the audit
// price even when the ledger write has not been confirmed.
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
	reservation.mu.Lock()
	defer reservation.mu.Unlock()
	if reservation.settled {
		return reservation.priced, true
	}
	if reservation.pending {
		result = reservation.priced
	}
	reservation.pending, reservation.priced = true, result
	var err error
	for range 3 {
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), billingReleaseTimeout)
		err = settler.SettleApiKeyBilling(settleCtx, reservation.KeyID, reservation.EventID, result.CostInUSDTicks)
		cancel()
		if err == nil {
			break
		}
	}
	if err != nil {
		AnnotateRequest(ctx, "billing_status", "pending")
		slog.Error("API key settlement pending reconciliation", "error", err, "key_id", reservation.KeyID, "event_id", reservation.EventID, "amount_ticks", result.CostInUSDTicks)
		return result, false
	}
	reservation.settled, reservation.pending = true, false
	AnnotateRequest(ctx, "billing_status", "settled")
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
		body, err := readBillingBody(r)
		if err != nil {
			writeAPIKeyError(w, http.StatusBadRequest, err.Error(), "invalid_request_body")
			return
		}
		if len(body) == 0 {
			next(w, r)
			return
		}
		if !json.Valid(body) {
			writeAPIKeyError(w, http.StatusBadRequest, "Invalid JSON request body", "invalid_request_body")
			return
		}
		reservation, priced := pricing.EstimateTextReservationFromBody(body)
		if !priced {
			// An unpriced model cannot be estimated, and guessing a rate would be
			// worse than not holding a budget for it.
			next(w, r)
			return
		}
		requestID := GetRequestID(r.Context())
		if requestID == "" {
			requestID = GenerateTraceID()
		}
		eventID := billingledger.EventID(time.Now(), requestID)
		if durable, ok := apiKeyBillingStore.(interface{ Ready() error }); ok {
			if err := durable.Ready(); err != nil {
				slog.Error("Billing admission unavailable", "error", err)
				writeAPIKeyError(w, http.StatusServiceUnavailable, "API key billing check unavailable", "billing_unavailable")
				return
			}
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
			if held.Settled() || held.Pending() {
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

// readBillingBody restores accepted bytes and rejects incomplete estimation.
func readBillingBody(r *http.Request) ([]byte, error) {
	if r == nil || r.Body == nil {
		return nil, nil
	}
	original := r.Body
	defer original.Close()
	raw, err := io.ReadAll(original)
	if err != nil {
		return nil, fmt.Errorf("Could not read request body")
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	return raw, nil
}
