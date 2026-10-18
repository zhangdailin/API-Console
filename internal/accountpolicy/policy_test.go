package accountpolicy

import (
	"errors"
	"strings"
	"testing"
	"time"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func grokBuildAccount() *store.Account {
	return &store.Account{ID: 1, AccountType: "grok", CredentialType: "oauth", GrokProvider: "build", OAuthAccessToken: "token"}
}

// TestClassify_RefusedCredentialNeedsLogin pins the verdict for a OAuth credential the
// upstream refused: the account is held, the operator is told to re-login, and
// the reason is persisted together with the status.
func TestClassify_RefusedCredentialNeedsLogin(t *testing.T) {
	acc := grokBuildAccount()
	v := Classify(acc, errors.New("401: grok session unauthenticated"), "grok-4.6")

	testutil.Equal(t, v.Scope, ScopeCredential)
	testutil.Falsef(t, !v.NeedsLogin || (v.Scope != ScopeAccount && v.Scope != ScopeCredential), "verdict must require a login and hold the account: %+v", v)
	testutil.Falsef(t, v.Status != "401" || !strings.Contains(v.Message, "Build OAuth") || strings.Contains(v.Message, "Cookie"), "verdict must identify the Build OAuth re-login without a retired cookie hint: %+v", v)
	testutil.Equal(t, v.Cooldown, CredentialReverify)
	v.Apply(acc)
	testutil.Equal(t, acc.AuthStatus, store.AccountAuthStatusReauthRequired)
}

// TestClassify_ModelScopedFailureKeepsAccount covers the P0 rule: a complaint
// about one model must not take the whole account out of the pool.
func TestClassify_ModelScopedFailureKeepsAccount(t *testing.T) {
	acc := grokBuildAccount()
	for _, message := range []string{
		"workbuddy API error: status=200, code=6004, message=usage exceeds frequency limit",
		"qoder agent limit reached; resets at 2026-09-27T19:47:13Z",
		`qoder upstream rejected the credential: {"agentLimitResetTime":1790538433100}`,
		"qoder gateway is busy: serviceAvailable=false retryAfterSeconds=29",
		"404: model is not found",
		"model not found: grok-4.6",
		"no_implementation_available for grok-4.6",
		"context_window_exceeded",
		"requested base model not allowed for this account",
	} {
		v := Classify(acc, errors.New(message), "grok-4.6")
		testutil.Equal(t, v.Scope, ScopeModel)
		testutil.Equal(t, v.Status, "")
		testutil.Falsef(t, v.Scope == ScopeAccount || v.Scope == ScopeCredential, "%q: a model-scoped failure must not hold the account", message)
		testutil.Equal(t, v.Model, "grok-4.6")
	}
}

// TestClassify_RateLimitIsAccountScopedWithShortCooldown keeps throttling a
// temporary, account-wide condition.
func TestClassify_RateLimitIsAccountScopedWithShortCooldown(t *testing.T) {
	v := Classify(grokBuildAccount(), errors.New("429: too many requests"), "grok-4.6")
	testutil.Equal(t, v.Scope, ScopeAccount)
	testutil.Equal(t, v.Status, "429")
	testutil.Equal(t, v.Cooldown, CooldownRateLimit)
	testutil.False(t, v.NeedsLogin, "throttling must not require a login")
}

// TestClassify_SuccessStampsVerdict keeps "never checked" distinguishable from
// "checked and healthy": the success verdict must stamp VerifiedAt.
func TestClassify_SuccessStampsVerdict(t *testing.T) {
	acc := grokBuildAccount()
	verdict := Classify(acc, nil, "grok-4.6")
	testutil.Falsef(t, verdict.Status != "" || (verdict.Scope != ScopeNone && verdict.Scope != ScopeModel), "success verdict is not healthy: %+v", verdict)
	verdict.Apply(acc)
	testutil.False(t, acc.VerifiedAt.IsZero(), "a success verdict must stamp VerifiedAt")
	testutil.Equal(t, acc.StatusCode, "")
	testutil.Equal(t, acc.StatusMessage, "")
}

// TestApply_KeepsStatusAndReasonTogether is the invariant the account table
// depends on: a reason never outlives its status.
func TestApply_KeepsStatusAndReasonTogether(t *testing.T) {
	acc := grokBuildAccount()
	acc.StatusCode = "429"
	acc.StatusMessage = "old reason"
	acc.LastAttempt = time.Now().Add(-time.Hour)

	Success(time.Now()).Apply(acc)
	testutil.Equal(t, acc.StatusCode, "")
	testutil.Equal(t, acc.StatusMessage, "")
	testutil.Falsef(t, !acc.LastAttempt.IsZero(), "recovery must clear the attempt stamp, got %v", acc.LastAttempt)

	Classify(acc, errors.New("429: slow down"), "").Apply(acc)
	testutil.Falsef(t, acc.StatusCode != "429" || acc.StatusMessage == "", "failure verdict lost status/reason: %+v", acc)
	testutil.False(t, acc.LastAttempt.IsZero(), "failure verdict must anchor the cooldown")
}

// TestAccountLifecycle pins hold/expiry behaviour shared by pool and scheduler.
func TestAccountLifecycle(t *testing.T) {
	rejected := grokBuildAccount()
	Classify(rejected, errors.New("401: grok session unauthenticated"), "").Apply(rejected)
	now := rejected.VerifiedAt
	testutil.False(t, !AccountHeld(rejected, now), "a freshly rejected credential must hold the account")
	testutil.False(t, NeedsReverify(rejected, now.Add(time.Minute)), "a refused credential must not be re-asked inside the revertify window")
	testutil.False(t, !NeedsReverify(rejected, now.Add(CredentialReverify)), "the credential must be re-asked once its window is over")
	testutil.False(t, !NeedsReverify(&store.Account{StatusCode: "401"}, now), "a 401 without a verdict stamp must be due immediately")
	testutil.NotEqual(t, AccountHeld(rejected, now.Add(24*time.Hour)), false)

	healthy := grokBuildAccount()
	Success(now).Apply(healthy)
	testutil.False(t, AccountHeld(healthy, now), "a healthy account must never be held")
}

// TestCooldownFor_MatchesPoolValues guards the numbers the pool already relies on.
func TestCooldownFor_MatchesPoolValues(t *testing.T) {
	cases := []struct {
		acc  *store.Account
		want time.Duration
	}{
		{&store.Account{StatusCode: "401"}, 30 * time.Minute},
		{&store.Account{StatusCode: "429"}, 30 * time.Second},
		{&store.Account{StatusCode: "402"}, 24 * time.Hour},
		{&store.Account{StatusCode: "402", AccountType: "qoder"}, 24 * time.Hour},
		// A WorkBuddy account reaches status 402 only when its allowance is gone
		// (a model-scoped refusal writes no status), so it is held like any other
		// payment verdict — and released early by isAccountAvailable once
		// QuotaResetAt says the allowance is back.
		{&store.Account{StatusCode: "402", AccountType: "workbuddy"}, 24 * time.Hour},
		{&store.Account{StatusCode: "403"}, 24 * time.Hour},
		{&store.Account{StatusCode: "403", AccountType: "grok"}, 10 * time.Minute},
		{&store.Account{StatusCode: "weird"}, 5 * time.Minute},
	}
	for _, tc := range cases {
		testutil.Equal(t, CooldownFor(tc.acc), tc.want)
	}
}

func TestRateLimitCooldownIsBoundedExponential(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	failures := []int{1, 2, 3, 6, 7, 20}
	for i, failureCount := range failures {
		testutil.Equal(t, RateLimitCooldown(failureCount), want[i])
	}
	testutil.Equal(t, BoundRateLimitCooldown(2*time.Hour), 30*time.Minute)
}

func TestAccountHeldUsesLaterBoundedReset(t *testing.T) {
	now := time.Now()
	acc := &store.Account{StatusCode: "429", LastAttempt: now, RateLimitFailures: 1, QuotaResetAt: now.Add(10 * time.Minute)}
	testutil.False(t, !AccountHeld(acc, now.Add(time.Minute)), "quota reset later than exponential cooldown must keep account held")
	testutil.False(t, AccountHeld(acc, now.Add(11*time.Minute)), "account should recover after the later reset")
}

// TestAccountHeld_429IgnoresBillingCycleReset is the regression test for the
// WorkBuddy outage of 2026-09-21.
//
// The quota sync writes the free plan's billing-cycle end into the same
// QuotaResetAt field a throttle uses for its retry-after, so a single one-minute
// 429 ("code=14003 too many requests") was read as "hold this account until the
// cycle resets" — days away. Every WorkBuddy account that hit one 429 left the
// pool for the rest of the month, the channel answered 503 to every request, and
// the log showed only eighteen 429s against 116 failures. A rate limit is a short
// capacity problem, so its hold is capped at the same ceiling RateLimitCooldown
// uses; a genuinely longer 402 allowance verdict is untouched.
func TestAccountHeld_429IgnoresBillingCycleReset(t *testing.T) {
	now := time.Now()
	cycleEnd := now.Add(9 * 24 * time.Hour) // the free-plan boundary the upstream reports

	acc := &store.Account{
		AccountType:       "workbuddy",
		StatusCode:        "429",
		LastAttempt:       now,
		RateLimitFailures: 2,
		QuotaResetAt:      cycleEnd,
	}

	if !AccountHeld(acc, now.Add(time.Minute)) {
		// One minute is well inside both the exponential cooldown and the cap.
		t.Fatal("a freshly rate-limited account must stay held during its cooldown")
	}
	if AccountHeld(acc, now.Add(CooldownRateLimitMax+time.Minute)) {
		t.Fatalf("429 held for %v; a billing-cycle reset must not extend a rate limit past %v",
			9*24*time.Hour, CooldownRateLimitMax)
	}
	testutil.False(t, AccountHeld(acc, now.Add(2*time.Hour)), "account should be back in rotation within the rate-limit ceiling")

	// The same far-future reset on a 402 is a real allowance verdict: it must keep
	// the account parked, or a spent account is offered again on every request.
	spent := &store.Account{
		AccountType:  "workbuddy",
		StatusCode:   "402",
		LastAttempt:  now,
		QuotaResetAt: cycleEnd,
	}
	testutil.False(t, !AccountHeld(spent, now.Add(2*time.Hour)), "a spent allowance must still hold the account until its reset")
}

// TestAccountHeld_429KeepsShortRetryAfter pins the other direction: the ceiling
// bounds an over-long reset, it does not replace a shorter one the upstream
// actually stated.
func TestAccountHeld_429KeepsShortRetryAfter(t *testing.T) {
	now := time.Now()
	acc := &store.Account{StatusCode: "429", LastAttempt: now, RateLimitFailures: 1, QuotaResetAt: now.Add(20 * time.Minute)}
	testutil.False(t, !AccountHeld(acc, now.Add(10*time.Minute)), "a stated 20m retry-after must still hold the account past its 30s exponential cooldown")
	testutil.False(t, AccountHeld(acc, now.Add(21*time.Minute)), "account should recover once the stated retry-after passed")
}

// TestClassify_WorkBuddyPaymentRefusalEnablesFreeOnlyMode pins the distinction:
// a spent WorkBuddy package remains selectable, but the handler permits only the
// explicitly confirmed free models for that account.
func TestClassify_WorkBuddyPaymentRefusalEnablesFreeOnlyMode(t *testing.T) {
	acc := &store.Account{ID: 1, AccountType: "workbuddy", Enabled: true}
	verdict := Classify(acc, errors.New("workbuddy API error: status=429 message=Credits exhausted code=14018"), "claude-sonnet-4.5")

	testutil.Equal(t, verdict.Scope, ScopeAccount)
	testutil.Equal(t, verdict.Status, store.AccountStatusWorkBuddyQuotaExhausted)
	verdict.Apply(acc)
	testutil.False(t, AccountHeld(acc, time.Now()), "a WorkBuddy quota-exhausted account must remain selectable for free models")
}

func TestClassifyInferenceCapPreservesStatedCooldown(t *testing.T) {
	wait := 17*time.Hour + 59*time.Minute
	acc := &store.Account{ID: 2, AccountType: "cline", Enabled: true}
	verdict := Classify(acc, retryAfterTestError{wait: wait}, "model-a")
	testutil.Equal(t, verdict.Scope, ScopeAccount)
	testutil.Equal(t, verdict.Status, "429")
	testutil.Equal(t, verdict.Cooldown, wait)
	verdict.Apply(acc)
	testutil.False(t, !AccountHeld(acc, time.Now().Add(time.Hour)), "inference cap account was released by the generic 30m throttle ceiling")
	remaining := time.Until(acc.QuotaResetAt)
	testutil.Falsef(t, remaining < wait-time.Second || remaining > wait+time.Second, "quota reset remaining=%v want %v", remaining, wait)
}

func TestClassifyQoderDailyCountHoldsUntilReset(t *testing.T) {
	acc := &store.Account{ID: 16, AccountType: "qoder", Enabled: true,
		QoderQuota: store.QoderQuotaSnapshot{ResetAt: time.Now().Add(8 * time.Hour)}}
	v := Classify(acc, errors.New("qoder upstream rejected the credential: Billing daily count exceeded"), "efficient")
	testutil.Falsef(t, v.Scope != ScopeAccount || v.Status != "429" || !v.Retryable || !v.SwitchAccount || v.Cooldown < time.Hour, "daily count verdict = %+v", v)
	v.Apply(acc)
	testutil.False(t, !AccountHeld(acc, time.Now().Add(time.Hour)), "daily limit must not be released after an ordinary 30s throttle")
}

func TestClassifyQoderEntitlementOnlyBlocksCurrentModel(t *testing.T) {
	acc := &store.Account{ID: 18, AccountType: "qoder", Enabled: true}
	v := Classify(acc, errors.New("qoder account has no usable plan or allowance; the model requires a subscription (upstream code=112)"), "efficient")
	testutil.Falsef(t, v.Scope != ScopeModel || v.Model != "efficient" || v.Status != "" || !v.Retryable || !v.SwitchAccount || v.Cooldown <= 0, "entitlement verdict = %+v", v)
}

type retryAfterTestError struct{ wait time.Duration }

func (e retryAfterTestError) Error() string             { return "cline inference cap reached" }
func (e retryAfterTestError) RetryAfter() time.Duration { return e.wait }

func TestClassifyCline403EntitlementIsModelScoped(t *testing.T) {
	acc := &store.Account{ID: 2, AccountType: "cline", Enabled: true}
	verdict := Classify(acc, errors.New(`cline API error: POST /chat/completions returned HTTP 403: {"error":"ENTITLEMENT_ERROR","message":"user is not subscribed to required model plan"}`), "paid-model")
	testutil.Falsef(t, verdict.Scope != ScopeModel || verdict.Status != "" || verdict.NeedsLogin || !verdict.SwitchAccount, "verdict=%+v", verdict)
}

// TestCredentialMessageIsProviderAware keeps the operator instruction concrete.
func TestCredentialMessageIsProviderAware(t *testing.T) {
	grokVerdict := Classify(grokBuildAccount(), errors.New("401: unauthenticated"), "")
	testutil.MustContainAll(t, grokVerdict.Message, "重新完成官方登录", "Build OAuth")
	other := Classify(&store.Account{AccountType: "workbuddy"}, errors.New("401: expired"), "")
	testutil.Falsef(t, other.Message == "" || other.NeedsLogin == false, "workbuddy verdict = %+v", other)
}

// TestClassify_WorkBuddyCreditExhaustionParksTheAccount is the regression test for
// the outage the model-scoped rule produced.
//
// The upstream's real refusal for a spent allowance is code 14018, whose text is
// "Credits exhausted. Please visit the link below to purchase add-on packs". That
// is a fact about the whole account — it is returned for every model — but it was
// read as a model-scoped payment refusal, so the account stayed in rotation, every
// request retried the whole pool, and the account table carried no reason for it.
func TestClassify_WorkBuddyCreditExhaustionEnablesFreeOnlyMode(t *testing.T) {
	// The production message, verbatim in shape: the upstream wraps it in JSON and
	// the transport wraps that in a status.
	production := `workbuddy API error: status=429, message={"error":{"data":{"code":14018,` +
		`"msg":"Credits exhausted. Please visit the link below to purchase add-on packs and ` +
		`get more credits: https://www.codebuddy.ai/profile/usage ","requestId":"abc"}}}`

	acc := &store.Account{ID: 1, AccountType: "workbuddy", Enabled: true}
	verdict := Classify(acc, errors.New(production), "fast-model")

	testutil.Equal(t, verdict.Scope, ScopeAccount)
	testutil.Equal(t, verdict.Status, store.AccountStatusWorkBuddyQuotaExhausted)
	verdict.Apply(acc)
	testutil.False(t, AccountHeld(acc, time.Now()), "credit exhaustion must not hide the account from confirmed free models")
	// The reason reaches the operator, including what to do about it.
	testutil.MustContain(t, acc.StatusMessage, "codebuddy.ai/profile/usage")
}

// TestIsCreditExhaustion_SeparatesTheTwoRefusals pins the distinction the rule
// rests on: both refusals arrive as 402, and only the wording says which one is
// about the account rather than about one request.
func TestIsCreditExhaustion_SeparatesTheTwoRefusals(t *testing.T) {
	exhausted := []string{
		"workbuddy API error: status=429, message=...Credits exhausted. Please visit the link below...",
		"status=402 no AI credits remaining",
		"available funding is insufficient to complete this request",
		"402 out of credits",
	}
	for _, message := range exhausted {
		testutil.CheckFalsef(t, !apperrors.IsCreditExhaustion(message), "IsCreditExhaustion(%q) = false, want true", message)
	}
	modelScoped := []string{
		"workbuddy API error: status=402 message=insufficient credits for model",
		"status=402 message=this model requires a paid plan",
		"workbuddy API error: status=429, code=14003, message=too many requests",
	}
	for _, message := range modelScoped {
		testutil.CheckFalsef(t, apperrors.IsCreditExhaustion(message), "IsCreditExhaustion(%q) = true, want false", message)
	}
}

// hintedError reproduces the shape production actually delivers: the channel's
// own error type carries the upstream's retry hint, which is what let the generic
// retry-after branch outrank the credit-exhaustion verdict.
type hintedError struct {
	message string
	wait    time.Duration
}

func (e hintedError) Error() string             { return e.message }
func (e hintedError) RetryAfter() time.Duration { return e.wait }

// TestClassify_SpentBalanceOutranksTheRetryHint is the regression test for the
// WorkBuddy free-only state being lost in production while the unit tests stayed
// green.
//
// The channel reports a spent balance as business code 14018 under a 429, and its
// error type implements RetryAfter() -- so the generic retry-after branch parked
// the account as an ordinary rate limit. A plain 429 is re-admitted as fully
// capable when its cooldown elapses, which is how a metered request reached an
// account with nothing left to spend.
func TestClassify_SpentBalanceOutranksTheRetryHint(t *testing.T) {
	production := `workbuddy API error: status=429, message={"error":{"data":{"code":14018,` +
		`"msg":"Credits exhausted. Please visit the link below to purchase add-on packs and ` +
		`get more credits: https://www.codebuddy.ai/profile/usage ","requestId":"dedb18a9"}}}`

	for name, wait := range map[string]time.Duration{"with a retry hint": 30 * time.Second, "with no hint": 0} {
		t.Run(name, func(t *testing.T) {
			acc := &store.Account{ID: 60, AccountType: "workbuddy", Enabled: true}
			verdict := Classify(acc, hintedError{message: production, wait: wait}, "hy3")

			testutil.Equal(t, verdict.Status, store.AccountStatusWorkBuddyQuotaExhausted)
			testutil.Equal(t, verdict.Scope, ScopeAccount)
			verdict.Apply(acc)
			testutil.False(t, AccountHeld(acc, time.Now()), "a spent account must stay selectable for its confirmed free models")
		})
	}
}

// The other direction: a real throttle that carries a hint must keep its own
// verdict, or every rate limit would be filed as an exhausted balance.
func TestClassify_PlainThrottleKeepsItsRetryHint(t *testing.T) {
	acc := &store.Account{ID: 60, AccountType: "workbuddy", Enabled: true}
	verdict := Classify(acc, hintedError{
		message: "workbuddy API error: status=429, message=too many requests, please slow down",
		wait:    2 * time.Minute,
	}, "hy3")

	testutil.Equal(t, verdict.Status, "429")
	testutil.Equal(t, verdict.Cooldown, 2*time.Minute)
}

// A spent balance is also not a payment refusal: it must not be answered as a
// per-request 402 hold, which parks the whole account for a day without leaving
// its free tier reachable.
func TestClassify_SpentBalanceOnQoderKeepsTheFreeTierState(t *testing.T) {
	acc := &store.Account{ID: 16, AccountType: "qoder", Enabled: true}
	verdict := Classify(acc, hintedError{
		message: "qoder API error: status=429, message=no AI credits remaining",
		wait:    30 * time.Second,
	}, "qwen3.8-flash")

	testutil.Equal(t, verdict.Status, store.AccountStatusQoderQuotaExhausted)
}

// Billing timestamps must neither shorten daily holds nor strand credentials
// until subscription expiry, including records written before this fix.
func TestQoderDailyCountIgnoresBillingReset(t *testing.T) {
	for _, billing := range []time.Duration{-time.Hour, 8 * time.Hour, 14 * 24 * time.Hour} {
		t.Run(billing.String(), func(t *testing.T) {
			acc := &store.Account{AccountType: "qoder", Enabled: true,
				QuotaResetAt: time.Now().Add(billing),
				QoderQuota:   store.QoderQuotaSnapshot{ResetAt: time.Now().Add(billing)}}
			v := Classify(acc, errors.New("Billing daily count exceeded"), "efficient")
			testutil.Equal(t, v.Cooldown, 24*time.Hour)
			v.Apply(acc)
			testutil.Equal(t, acc.QuotaResetAt, v.At.Add(24*time.Hour))
			testutil.Equal(t, CooldownFor(acc), 24*time.Hour)
			// Simulate a legacy record or a later subscription quota refresh.
			acc.QuotaResetAt = v.At.Add(billing)
			testutil.True(t, AccountHeld(acc, v.At.Add(23*time.Hour)))
			testutil.False(t, AccountHeld(acc, v.At.Add(24*time.Hour)))
			testutil.False(t, AccountHeld(acc, v.At.Add(25*time.Hour)))
		})
	}
}
