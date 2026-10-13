package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/audit"
	"orchids-api/internal/testutil"
	"testing"
	"time"
)

type retryLedger struct {
	stubBillingLedger
	calls int
	fail  int
}

func (s *retryLedger) SettleApiKeyBilling(ctx context.Context, id int64, event string, amount int64) error {
	s.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.calls <= s.fail {
		return context.DeadlineExceeded
	}
	return s.stubBillingLedger.SettleApiKeyBilling(ctx, id, event, amount)
}

func TestSettlementSurvivesCancellationAndTransientFailure(t *testing.T) {
	ledger := &retryLedger{fail: 1}
	held := &BillingReservation{KeyID: 1, EventID: "request"}
	ctx, cancel := context.WithCancel(WithBillingReservation(context.Background(), held))
	cancel()
	_, ok := SettleAPIKeyBilling(ctx, ledger, "grok-4.6", audit.UsageSourceUpstream, 100, 0, 50)
	testutil.True(t, ok)
	testutil.True(t, held.Settled())
	testutil.Equal(t, ledger.calls, 2)
}

func TestFailedSettlementIsRetryableAndDoesNotRelease(t *testing.T) {
	ledger := &retryLedger{fail: 3}
	ledger.limit = 1_000_000_000_000
	var saved context.Context
	handler := APIKeyBillingReservation(func(_ http.ResponseWriter, r *http.Request) {
		saved = r.Context()
		_, ok := SettleAPIKeyBilling(saved, ledger, "grok-4.6", audit.UsageSourceUpstream, 100, 0, 50)
		testutil.False(t, ok)
	}, ledger, time.Minute)
	handler(httptest.NewRecorder(), billingRequest("POST", "/grok/v1/messages", billingChatBody, &APIKeyPrincipal{ID: 1, BillingLimitUSDTicks: ledger.limit}, "retry"))
	testutil.Equal(t, len(ledger.releases), 0)
	testutil.True(t, BillingReservationFrom(saved).Pending())
	_, ok := SettleAPIKeyBilling(saved, ledger, "grok-4.6", audit.UsageSourceUpstream, 100, 0, 50)
	testutil.True(t, ok)
	testutil.Equal(t, len(ledger.settles), 1)
}

func TestEmptyAdminPasswordCannotAuthenticate(t *testing.T) {
	handler := SessionAuthDynamic(func() (string, string) { return "", "" }, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	req := httptest.NewRequest("GET", "/api/config/list", nil)
	req.SetBasicAuth("any", "")
	out := httptest.NewRecorder()
	handler(out, req)
	testutil.Equal(t, out.Code, 401)
}
