package middleware

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/audit"
	"orchids-api/internal/pricing"
	"orchids-api/internal/testutil"
)

// stubBillingLedger is an in-memory stand-in for the Redis ledger. It applies
// the same limit rule so the middleware's decision is observable without Redis.
type stubBillingLedger struct {
	limit      int64
	used       int64
	held       map[string]int64
	reserveErr error
	settleErr  error
	settles    []int64
	releases   []string
	// ctx captures the reserved request context so a test can settle outside the
	// handler that took the hold.
	ctx context.Context
}

func (s *stubBillingLedger) ReserveApiKeyBilling(_ context.Context, _ int64, eventID string, amount int64, _ time.Time) (bool, error) {
	if s.reserveErr != nil {
		return false, s.reserveErr
	}
	if s.held == nil {
		s.held = map[string]int64{}
	}
	var live int64
	for _, held := range s.held {
		live += held
	}
	if s.limit > 0 && s.used+live+amount > s.limit {
		return false, nil
	}
	s.held[eventID] = amount
	return true, nil
}

func (s *stubBillingLedger) SettleApiKeyBilling(_ context.Context, _ int64, eventID string, amount int64) error {
	if s.settleErr != nil {
		return s.settleErr
	}
	delete(s.held, eventID)
	s.used += amount
	s.settles = append(s.settles, amount)
	return nil
}

func (s *stubBillingLedger) ReleaseApiKeyBilling(_ context.Context, _ int64, eventID string) (bool, error) {
	if _, ok := s.held[eventID]; !ok {
		return false, nil
	}
	delete(s.held, eventID)
	s.releases = append(s.releases, eventID)
	return true, nil
}

func billingRequest(method, path, body string, principal *APIKeyPrincipal, requestID string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := context.WithValue(request.Context(), requestIDKey{}, requestID)
	ctx = context.WithValue(ctx, apiKeyPrincipalContextKey{}, principal)
	return request.WithContext(ctx)
}

const billingChatBody = `{"model":"grok-4.6","max_tokens":512,"messages":[{"role":"user","content":"hello"}]}`

// TestAPIKeyBillingReservationSettles covers the happy path: the hold exists for
// the handler, the handler's body is intact, and settling books the real cost
// instead of leaving the hold behind.
func TestAPIKeyBillingReservationSettles(t *testing.T) {
	ledger := &stubBillingLedger{limit: 1_000_000_000_000}
	principal := &APIKeyPrincipal{ID: 7, BillingLimitUSDTicks: 1_000_000_000_000}

	var (
		seenBody   string
		seenHeld   *BillingReservation
		settled    pricing.Result
		settleOK   bool
		handlerRun bool
	)
	handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
		handlerRun = true
		raw := make([]byte, 0, len(billingChatBody))
		buf := make([]byte, 64)
		for {
			n, err := r.Body.Read(buf)
			raw = append(raw, buf[:n]...)
			if err != nil {
				break
			}
		}
		seenBody = string(raw)
		seenHeld = BillingReservationFrom(r.Context())
		settled, settleOK = SettleAPIKeyBilling(r.Context(), ledger, "grok-4.6", audit.UsageSourceUpstream, 1000, 0, 500)
	}, ledger, time.Minute)

	recorder := httptest.NewRecorder()
	handler(recorder, billingRequest(http.MethodPost, "/grok/v1/chat/completions", billingChatBody, principal, "req-1"))

	testutil.True(t, handlerRun, "handler must run for a reservation that fits")
	testutil.Equal(t, seenBody, billingChatBody)
	testutil.Falsef(t, seenHeld == nil || seenHeld.KeyID != 7 || seenHeld.EventID != "req-1" || seenHeld.Amount <= 0, "reservation in context = %#v", seenHeld)
	testutil.Falsef(t, !settleOK || settled.Model != "grok-4.6", "settle = %#v, %v", settled, settleOK)
	// 1000 input at 20000 ticks + 500 output at 60000 ticks.
	want := int64(1000*20000 + 500*60000)
	testutil.Falsef(t, settled.CostInUSDTicks != want, "cost = %d, want %d", settled.CostInUSDTicks, want)
	testutil.Falsef(t, len(ledger.settles) != 1 || ledger.settles[0] != settled.CostInUSDTicks || ledger.used != settled.CostInUSDTicks, "ledger after settle = %#v", ledger)
	testutil.Equal(t, len(ledger.held), 0)
	testutil.Equal(t, len(ledger.releases), 0)
}

// TestAPIKeyBillingReservationReleasesUnsettledRequest checks the deferred
// cleanup: a request that never settled must not keep holding budget.
func TestAPIKeyBillingReservationReleasesUnsettledRequest(t *testing.T) {
	ledger := &stubBillingLedger{limit: 1_000_000_000_000}
	principal := &APIKeyPrincipal{ID: 9, BillingLimitUSDTicks: 1_000_000_000_000}

	handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, ledger, time.Minute)

	recorder := httptest.NewRecorder()
	handler(recorder, billingRequest(http.MethodPost, "/grok/v1/messages", billingChatBody, principal, "req-release"))

	testutil.Equal(t, len(ledger.releases), 1)
	testutil.Equal(t, ledger.releases[0], "req-release")
	testutil.Equal(t, ledger.used, 0)
	testutil.Equal(t, len(ledger.held), 0)
}

// TestAPIKeyBillingReservationRefusesOverLimit pins the 402 contract and that a
// refused request never reaches its handler.
func TestAPIKeyBillingReservationRefusesOverLimit(t *testing.T) {
	ledger := &stubBillingLedger{limit: 100}
	principal := &APIKeyPrincipal{ID: 11, BillingLimitUSDTicks: 100}

	handlerRun := false
	handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
		handlerRun = true
	}, ledger, time.Minute)

	recorder := httptest.NewRecorder()
	handler(recorder, billingRequest(http.MethodPost, "/grok/v1/chat/completions", billingChatBody, principal, "req-over"))

	testutil.False(t, handlerRun, "a refused reservation must not call the handler")
	testutil.Equal(t, recorder.Code, http.StatusPaymentRequired)
	testutil.Equal(t, recorder.Header().Get("WWW-Authenticate"), "Bearer")
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	err := json.Unmarshal(recorder.Body.Bytes(), &envelope)
	testutil.CheckNoError(t, err)
	testutil.Equal(t, envelope.Error.Code, "billing_limit_exceeded")
	testutil.Equal(t, envelope.Error.Message, "API key billing limit exceeded")
	testutil.Equal(t, envelope.Error.Type, "insufficient_quota")
	testutil.Equal(t, len(ledger.held), 0)
}

// TestAPIKeyBillingReservationFailsClosedOnLedgerError keeps a Redis outage from
// turning a budgeted key into an unmetered one.
func TestAPIKeyBillingReservationFailsClosedOnLedgerError(t *testing.T) {
	ledger := &stubBillingLedger{limit: 1_000_000_000_000, reserveErr: context.DeadlineExceeded}
	principal := &APIKeyPrincipal{ID: 12, BillingLimitUSDTicks: 1_000_000_000_000}

	handlerRun := false
	handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
		handlerRun = true
	}, ledger, time.Minute)

	recorder := httptest.NewRecorder()
	handler(recorder, billingRequest(http.MethodPost, "/grok/v1/chat/completions", billingChatBody, principal, "req-err"))

	testutil.False(t, handlerRun, "a failing ledger must not let the request through unmetered")
	testutil.Equal(t, recorder.Code, http.StatusServiceUnavailable)
}

// TestAPIKeyBillingReservationSkipsUnbilledRequests keeps the middleware off
// every request that has nothing to price.
func TestAPIKeyBillingReservationSkipsUnbilledRequests(t *testing.T) {
	cases := []struct {
		name      string
		method    string
		path      string
		body      string
		principal *APIKeyPrincipal
	}{
		{"unlimited key", http.MethodPost, "/grok/v1/chat/completions", billingChatBody, &APIKeyPrincipal{ID: 1}},
		{"get", http.MethodGet, "/grok/v1/chat/completions", "", &APIKeyPrincipal{ID: 1, BillingLimitUSDTicks: 10}},
		{"empty body", http.MethodPost, "/grok/v1/chat/completions", "", &APIKeyPrincipal{ID: 1, BillingLimitUSDTicks: 10}},
		{"non inference path", http.MethodPost, "/grok/v1/models", `{"model":"grok-4.6"}`, &APIKeyPrincipal{ID: 1, BillingLimitUSDTicks: 10}},
		{"unpriced model", http.MethodPost, "/grok/v1/chat/completions", `{"model":"gpt-5"}`, &APIKeyPrincipal{ID: 1, BillingLimitUSDTicks: 10}},
		{"no principal", http.MethodPost, "/grok/v1/chat/completions", billingChatBody, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := &stubBillingLedger{limit: 10}
			handlerRun := false
			handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
				handlerRun = true
			}, ledger, time.Minute)

			recorder := httptest.NewRecorder()
			handler(recorder, billingRequest(tc.method, tc.path, tc.body, tc.principal, "req-skip"))

			testutil.True(t, handlerRun, "handler must still run")
			testutil.Falsef(t, len(ledger.held) != 0 || len(ledger.settles) != 0 || len(ledger.releases) != 0, "ledger was touched: %#v", ledger)
		})
	}
}

// TestAPIKeyBillingReservationReleasesAfterClientDisconnect keeps the cleanup
// working when the request context is already cancelled.
func TestAPIKeyBillingReservationReleasesAfterClientDisconnect(t *testing.T) {
	ledger := &stubBillingLedger{limit: 1_000_000_000_000}
	principal := &APIKeyPrincipal{ID: 21, BillingLimitUSDTicks: 1_000_000_000_000}

	handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
		// The client disappears mid-request, which cancels r.Context().
		cancel := r.Context().Value(cancelKey{})
		if fn, ok := cancel.(context.CancelFunc); ok {
			fn()
		}
	}, ledger, time.Minute)

	request := billingRequest(http.MethodPost, "/grok/v1/chat/completions", billingChatBody, principal, "req-disconnect")
	ctx, cancel := context.WithCancel(request.Context())
	ctx = context.WithValue(ctx, cancelKey{}, cancel)
	handler(httptest.NewRecorder(), request.WithContext(ctx))

	testutil.Equal(t, len(ledger.releases), 1)
}

type cancelKey struct{}

// TestAPIKeyBillingReservationRestoresOversizedBody checks that a body past the
// estimation cap still reaches the handler byte for byte.
func TestAPIKeyBillingReservationRestoresOversizedBody(t *testing.T) {
	ledger := &stubBillingLedger{limit: 1_000_000_000_000}
	principal := &APIKeyPrincipal{ID: 31, BillingLimitUSDTicks: 1_000_000_000_000}

	var size int
	handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
		raw, err := readAllBody(r)
		testutil.CheckNoError(t, err, "handler could not read the body: %v")
		size = len(raw)
		res := BillingReservationFrom(r.Context())
		testutil.CheckFalse(t, res == nil, "oversized body was not reserved")
		// Settle so the successful reservation stays observable after the
		// middleware's deferred cleanup.
		SettleAPIKeyBilling(r.Context(), ledger, "grok-4.6", audit.UsageSourceUpstream, 10, 0, 10)
	}, ledger, time.Minute)

	body := `{"model":"grok-4.6","max_tokens":8,"messages":[{"role":"user","content":"` +
		strings.Repeat("x", maxBillingBodyBytes+1024) + `"}]}`
	handler(httptest.NewRecorder(), billingRequest(http.MethodPost, "/grok/v1/chat/completions", body, principal, "req-big"))

	testutil.Equal(t, size, len(body))
	testutil.Equal(t, len(ledger.settles), 1)
}

func readAllBody(r *http.Request) ([]byte, error) {
	var raw bytes.Buffer
	_, err := raw.ReadFrom(r.Body)
	return raw.Bytes(), err
}

// TestSettleAPIKeyBillingRules pins the pricing policy in one place.
func TestSettleAPIKeyBillingRules(t *testing.T) {
	principal := &APIKeyPrincipal{ID: 41, BillingLimitUSDTicks: 1_000_000_000_000}
	reserved := func(t *testing.T, ledger *stubBillingLedger) context.Context {
		t.Helper()
		request := billingRequest(http.MethodPost, "/grok/v1/chat/completions", billingChatBody, principal, "req-rules")
		handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
			// Hand the reserved context back to the test through the ledger.
			ledger.ctx = r.Context()
		}, ledger, time.Minute)
		handler(httptest.NewRecorder(), request)
		return ledger.ctx
	}

	t.Run("estimated usage is not billed", func(t *testing.T) {
		ledger := &stubBillingLedger{limit: 1_000_000_000_000}
		ctx := reserved(t, ledger)
		_, priced := SettleAPIKeyBilling(ctx, ledger, "grok-4.6", audit.UsageSourceEstimated, 1000, 0, 500)
		testutil.False(t, priced, "an estimated row must not be priced")
		testutil.Equal(t, len(ledger.settles), 0)
	})

	t.Run("unpriced model", func(t *testing.T) {
		ledger := &stubBillingLedger{limit: 1_000_000_000_000}
		ctx := reserved(t, ledger)
		_, priced := SettleAPIKeyBilling(ctx, ledger, "gpt-5", audit.UsageSourceUpstream, 1000, 0, 500)
		testutil.False(t, priced, "an unpriced model must not be priced")
		testutil.Equal(t, len(ledger.settles), 0)
	})

	t.Run("idempotent per event id", func(t *testing.T) {
		ledger := &stubBillingLedger{limit: 1_000_000_000_000}
		ctx := reserved(t, ledger)
		first, _ := SettleAPIKeyBilling(ctx, ledger, "grok-4.6", audit.UsageSourceUpstream, 1000, 0, 500)
		second, priced := SettleAPIKeyBilling(ctx, ledger, "grok-4.6", audit.UsageSourceUpstream, 1000, 0, 500)
		testutil.Falsef(t, !priced || first != second, "first=%#v second=%#v priced=%v", first, second, priced)
		testutil.Equal(t, len(ledger.settles), 1)
	})

	t.Run("without a reservation the cost is still reported", func(t *testing.T) {
		ledger := &stubBillingLedger{limit: 1_000_000_000_000}
		result, priced := SettleAPIKeyBilling(context.Background(), ledger, "grok-4.6", audit.UsageSourceUpstream, 1000, 0, 500)
		testutil.Falsef(t, !priced || result.Model != "grok-4.6", "result=%#v priced=%v", result, priced)
		testutil.Equal(t, len(ledger.settles), 0)
		testutil.Equal(t, ledger.used, 0)
	})

	t.Run("nil settler falls back to the wired ledger", func(t *testing.T) {
		ledger := &stubBillingLedger{limit: 1_000_000_000_000}
		SetAPIKeyBillingStore(ledger)
		t.Cleanup(func() { SetAPIKeyBillingStore(nil) })
		ctx := reserved(t, ledger)
		_, priced := SettleAPIKeyBilling(ctx, nil, "grok-4.6", audit.UsageSourceUpstream, 1000, 0, 500)
		testutil.False(t, !priced, "a wired ledger must be used")
		testutil.Equal(t, len(ledger.settles), 1)
	})

	t.Run("settle failure keeps the audit cost", func(t *testing.T) {
		ledger := &stubBillingLedger{limit: 1_000_000_000_000, settleErr: context.DeadlineExceeded}
		ctx := reserved(t, ledger)
		result, priced := SettleAPIKeyBilling(ctx, ledger, "grok-4.6", audit.UsageSourceUpstream, 1000, 0, 500)
		testutil.Falsef(t, !priced || result.CostInUSDTicks == 0, "result=%#v priced=%v", result, priced)
		// The hold stays claimed so a retry cannot charge twice.
		res := BillingReservationFrom(ctx)
		testutil.False(t, res == nil || !res.Settled(), "a failed settlement must still claim the reservation")
	})
}

// TestBillingReservationConcurrentSettleIsIdempotent covers the streaming path,
// where a settle call may race the request's own cleanup.
func TestBillingReservationConcurrentSettleIsIdempotent(t *testing.T) {
	held := &BillingReservation{KeyID: 1, EventID: "event", Amount: 10}
	const workers = 8
	claimed := make(chan bool, workers)
	done := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func() {
			_, first := held.claim(pricing.Result{Model: "grok-4.6", CostInUSDTicks: 5})
			claimed <- first
			<-done
		}()
	}
	winners := 0
	for i := 0; i < workers; i++ {
		if <-claimed {
			winners++
		}
	}
	close(done)
	testutil.Equal(t, winners, 1)
}

func TestBillingRequestPathMatching(t *testing.T) {
	priced := []string{"/grok/v1/chat/completions", "/grok/messages", "/grok/responses", "/qoder/responses/compact", "/cline/messages", "/workbuddy/v1/chat/completions"}
	for _, path := range priced {
		testutil.True(t, billingRequestPath(path), "billingRequestPath(%q) = false, want true")
	}
	for _, path := range []string{"/grok/v1/models", "/api/keys", "/grok/v1/tts", "/grok/v1/images/generations", "/", "/v1/responses", "/messages", "/workbuddy/messages/count_tokens", "/workbuddy/responses/id/cancel", "/grok/responses/id/input_items"} {
		testutil.Falsef(t, billingRequestPath(path), "billingRequestPath(%q) = true, want false", path)
	}
}
