// Package accountpolicy centralises how one upstream result becomes an account
// state change. Before this existed the same question — "is this account still
// usable, and for how long?" — was answered independently by the scheduler, the
// admin API and the account pool, which is how an account could look healthy in
// one place and unauthorized in another.
package accountpolicy

import (
	stderrors "errors"
	"strings"
	"time"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/store"
)

// Scope states how much of an account a failure invalidates.
type Scope string

const (
	// ScopeNone: the result does not change account state at all (a model
	// mismatch, a malformed request, a client-side error).
	ScopeNone Scope = "none"
	// ScopeModel: only the model that produced the result is exhausted or
	// throttled; the account stays usable for its other models.
	ScopeModel Scope = "model"
	// ScopeAccount: the account as a whole is temporarily unavailable (rate
	// limit, quota, transient upstream failure).
	ScopeAccount Scope = "account"
	// ScopeCredential: the credential itself was refused. The account cannot
	// recover by waiting; it needs a new login or a new cookie.
	ScopeCredential Scope = "credential"
)

// Cooldown windows. They mirror the account pool's long-standing values so the
// scheduler and the pool expire a verdict at the same moment.
const (
	CooldownAuth          = 5 * time.Minute
	CooldownRateLimitBase = 30 * time.Second
	CooldownRateLimitMax  = 30 * time.Minute
	// CooldownRateLimit is retained as the first-failure/default alias.
	CooldownRateLimit  = CooldownRateLimitBase
	CooldownPayment    = 24 * time.Hour
	CooldownBlocked    = 24 * time.Hour
	CooldownBlockedGro = 10 * time.Minute
	CooldownTransient  = 5 * time.Minute

	// CredentialReverify is how long a credential the upstream refused is kept
	// out of the refresh rotation before it is re-asked once, in case the
	// rejection was transient or the operator re-authenticated.
	CredentialReverify = 30 * time.Minute
)

// Verdict is the complete, self-describing outcome of one upstream result.
type Verdict struct {
	// Status is the account status code to persist ("" clears it).
	Status string
	// Message is the operator-facing reason. It never outlives the status.
	Message string
	// Scope says what the failure invalidates.
	Scope Scope
	// Retryable reports whether the same request may be attempted again.
	Retryable bool
	// SwitchAccount reports whether the pool should try a different account.
	SwitchAccount bool
	// NeedsLogin reports that waiting cannot help: a human must re-authorize.
	NeedsLogin bool
	// Cooldown is how long the account (or the model) stays withheld.
	Cooldown time.Duration
	// Model names the model the verdict is scoped to, when Scope is ScopeModel.
	Model string
	// ModelCooldownKind is what a ScopeModel cooldown means: a throttle that
	// clears by waiting, or a plan the account does not have. It is stored beside
	// the deadline because the selection layer cannot see the request that
	// recorded it, and the kind is what decides whether an emptied pool is
	// answered with "retry later" or with "this model is not available here".
	ModelCooldownKind store.ModelCooldownReason
	// At is when the result was observed; it anchors the cooldown.
	At time.Time
}

type retryAfterError interface {
	RetryAfter() time.Duration
}

// Success is the verdict for an upstream result that proved the credential
// works. It always stamps VerifiedAt so "never checked" stays distinguishable
// from "checked and healthy".
func Success(at time.Time) Verdict { return Verdict{Scope: ScopeNone, At: at} }

// Apply records the verdict on the account. It is the only place that moves the
// status, the reason and the verdict stamp together, so a partial write can
// never leave a reason behind without its status, or a stale status in place of
// a fresh one.
func (v Verdict) Apply(acc *store.Account) {
	if acc == nil {
		return
	}
	at := v.At
	if at.IsZero() {
		at = time.Now()
	}
	acc.StatusCode = strings.TrimSpace(v.Status)
	if v.NeedsLogin {
		acc.AuthStatus = store.AccountAuthStatusReauthRequired
	} else if acc.StatusCode == "" {
		acc.AuthStatus = store.AccountAuthStatusActive
	}
	if acc.StatusCode == "" {
		acc.StatusMessage = ""
		acc.LastAttempt = time.Time{}
		acc.RateLimitFailures = 0
	} else {
		acc.StatusMessage = strings.TrimSpace(v.Message)
		acc.LastAttempt = at
		if acc.StatusCode == "429" {
			acc.RateLimitFailures++
			if v.Cooldown > 0 {
				reset := at.Add(v.Cooldown)
				if reset.After(acc.QuotaResetAt) {
					acc.QuotaResetAt = reset
				}
			}
		}
	}
	// Any verdict — healthy or not — proves the credential was exercised, which
	// is what makes "never checked" distinguishable from "checked and healthy".
	acc.VerifiedAt = at
}

// ScopeForStatus maps an already-classified status code to the part of the
// account it invalidates, so callers that only have the code (a verifier that
// returned "429", an admin token probe) still produce a complete verdict.
func ScopeForStatus(status string) Scope {
	switch strings.TrimSpace(status) {
	case "401":
		return ScopeCredential
	case "402", "403", "404", "429", store.AccountStatusQoderQuotaExhausted, store.AccountStatusWorkBuddyQuotaExhausted:
		return ScopeAccount
	case "":
		return ScopeNone
	default:
		return ScopeAccount
	}
}

// Retryable derives whether the same request may be attempted again from the
// shared upstream-error classification. It exists so the request path and the
// scheduler read one rule instead of two: before this, the handler decided
// retries from category strings while the scheduler decided state from the
// policy, and the two could disagree about the same error.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	return apperrors.ClassifyUpstreamError(err.Error()).Retryable
}

// Classify turns an upstream error into a verdict.
func Classify(acc *store.Account, err error, model string) Verdict {
	if err == nil {
		return Success(time.Now())
	}
	message := strings.TrimSpace(err.Error())
	lower := strings.ToLower(message)
	now := time.Now()
	// An explicit inference-cap window is account-wide and may last hours;
	// unlike a generic rate limit, its stated reset must not be capped at 30m.
	if apperrors.IsClineInferenceCap(lower) {
		cooldown := CooldownRateLimit
		var hint retryAfterError
		if stderrors.As(err, &hint) && hint.RetryAfter() > 0 {
			cooldown = hint.RetryAfter()
		}
		return Verdict{Status: "429", Message: message, Scope: ScopeAccount,
			Retryable: true, SwitchAccount: true, Cooldown: cooldown, At: now}
	}

	// The Qoder daily *request count* can run out while the credit meter still
	// reports a positive balance. It is not an ordinary 30-second throttle:
	// keep this credential out of rotation until the next known reset, or use
	// a bounded day-long fallback when the upstream supplied no reset.
	if strings.EqualFold(accountType(acc), "qoder") && strings.Contains(lower, "billing daily count exceeded") {
		cooldown := CooldownPayment
		if acc != nil && !acc.QoderQuota.ResetAt.IsZero() && acc.QoderQuota.ResetAt.After(now) {
			cooldown = acc.QoderQuota.ResetAt.Sub(now)
		}
		return Verdict{Status: "429", Message: message, Scope: ScopeAccount,
			Retryable: true, SwitchAccount: true, Cooldown: cooldown, At: now}
	}

	// A plan/allowance refusal is about this account and this model, not the
	// caller's input or the credential itself. Cool down only this pairing and
	// allow the request to try another account with the required entitlement.
	// The cooldown is labelled as an entitlement so the pool can answer an
	// emptied one with "not available here" instead of "retry later".
	if strings.EqualFold(accountType(acc), "qoder") && strings.Contains(lower, "no usable plan or allowance") {
		return Verdict{Scope: ScopeModel, Message: message, Model: model,
			Retryable: true, SwitchAccount: true, Cooldown: CooldownPayment,
			ModelCooldownKind: store.ModelCooldownUnavailable, At: now}
	}

	// A queue/service refusal that names the whole upstream rather than this
	// account has to be decided before the retryAfter hint below. Qoder reports
	// 10605 as {"isQueued":true,"serviceAvailable":false,"retryAfterSeconds":30},
	// and the error carrying it implements RetryAfter() -- so the generic branch
	// used to win, park the account as "429" for 30s and rotate to the next one.
	// Every account then ate the same shared refusal in turn: the pool drained to
	// zero within seconds, with pool-empty alerts and a ~20% success rate, while
	// the upstream had only said that this model's queue was unavailable.
	//
	// The verdict is retryable but deliberately not switchable, and it records
	// nothing: the request waits out the upstream's own window and tries again on
	// the account it already holds. That is the only useful response to a
	// condition that is identical for every account -- rotating multiplies the
	// refusal, and failing instantly throws away requests that a short wait would
	// have served once the queue cleared.
	//
	// Cooldown is left zero on purpose: persisting a model cooldown here would
	// take this account, and then every other one, out of selection for the
	// window and turn the wait back into the fail-fast this is meant to replace.
	if isGlobalUpstreamRefusal(lower) {
		return Verdict{
			Scope:         ScopeModel,
			Message:       message,
			Model:         model,
			Retryable:     true,
			SwitchAccount: false,
			At:            now,
		}
	}

	// A refusal that is a verdict about the request itself — the safety review
	// rejecting the input, or the upstream naming an invalid parameter — has to
	// be decided before the generic branches below. Falling through would file
	// it as "unknown" with SwitchAccount=true, which is what walked one rejected
	// prompt across every account in the pool and left each of them stamped
	// "429" for a problem that no account can fix.
	//
	// The verdict records nothing on the account and never switches: the same
	// input fails identically everywhere, and the client gets a 400.
	if isClientRefusal(lower) {
		return Verdict{
			Scope:         ScopeNone,
			Message:       message,
			Retryable:     false,
			SwitchAccount: false,
			At:            now,
		}
	}
	// A 200 stream that delivered nothing is the upstream refusing quietly. It
	// is not an account problem either: replaying it only adds load to the
	// condition that produced the empty answer.
	if strings.Contains(lower, "empty upstream stream") {
		return Verdict{
			Scope:         ScopeNone,
			Message:       message,
			Retryable:     false,
			SwitchAccount: false,
			At:            now,
		}
	}

	// A spent balance is a fact about the account, and it has to be decided before
	// the generic retry-after branch below. WorkBuddy reports it as business code
	// 14018 ("Credits exhausted") under a 429, and its error type implements
	// RetryAfter() — so the generic branch won, parked the account as an ordinary
	// rate limit, and dropped the free-only capability state that is the only
	// reason such an account stays in the pool. Production then dispatched metered
	// traffic to an account with nothing left to spend, because a plain 429 is
	// re-admitted as fully capable once its cooldown elapses.
	if apperrors.IsCreditExhaustion(lower) {
		if verdict, ok := creditExhaustionVerdict(acc, message, err, now); ok {
			return verdict
		}
	}

	// A model-scoped complaint must not take the account out of service: the
	// other models of the same account remain usable.
	if isModelScopedFailure(lower) {
		return Verdict{
			Scope:         ScopeModel,
			Message:       message,
			Model:         model,
			Retryable:     Retryable(err),
			SwitchAccount: true,
			Cooldown:      rateLimitHint(err, CooldownRateLimit),
			// Most model-scoped complaints are a frequency limit that a wait
			// clears; the plan ones are not, and the pool needs to know which.
			ModelCooldownKind: modelCooldownKindFor(lower),
			At:                now,
		}
	}

	switch apperrors.ClassifyAccountStatus(message) {
	case "401":
		return Verdict{
			Status:    "401",
			Message:   credentialMessage(acc, message),
			Scope:     ScopeCredential,
			Retryable: Retryable(err),
			// A refused credential cannot serve this request, and the pool may
			// hold another one that can. The switch decision is read from this
			// verdict, so it has to say so here rather than leaving callers to
			// infer it from a second classifier.
			SwitchAccount: true,
			NeedsLogin:    true,
			Cooldown:      CredentialReverify,
			At:            now,
		}
	case "403", "404":
		cooldown := CooldownBlocked
		if isGrok(acc) {
			// Grok answers 403 for transient upstream denials, which clear quickly.
			cooldown = CooldownBlockedGro
		}
		return Verdict{
			Status: apperrors.ClassifyAccountStatus(message), Message: message,
			Scope: ScopeAccount, Retryable: Retryable(err), SwitchAccount: true,
			Cooldown: cooldown, At: now,
		}
	case "402":
		// A spent balance on a channel that has a free tier was already decided
		// above, whatever HTTP status or retry hint carried it. What remains here is
		// a per-request payment refusal ("insufficient credits for model"), which is
		// not a verdict about the account's whole balance, and the older persisted
		// "402" marker.
		//
		// An exhausted allowance is still a fact about the whole account within this
		// case: leaving it in rotation is what made every request retry a pool of
		// dead accounts and return an error with nothing in the account table to
		// explain it. Parking it stops the retries and puts the reason in front of
		// the operator; isAccountAvailable honours QuotaResetAt, so the account
		// returns when its allowance does.
		//
		// This is also the release path for a "402" persisted under the old rule,
		// which held nothing: such a marker is now held, but only until the reset
		// time it was written with has passed.
		cooldown := CooldownPayment
		return Verdict{
			Status: "402", Message: message,
			Scope: ScopeAccount, Retryable: Retryable(err), SwitchAccount: true,
			Cooldown: cooldown, At: now,
		}
	case "429":
		return Verdict{
			Status: "429", Message: message,
			Scope: ScopeAccount, Retryable: Retryable(err), SwitchAccount: true,
			Cooldown: rateLimitHint(err, CooldownRateLimit), At: now,
		}
	}

	// Everything else is transient: keep the account, retry elsewhere.
	return Verdict{
		Status: "", Message: "",
		Scope: ScopeNone, Retryable: Retryable(err), SwitchAccount: true,
		At: now,
	}
}

// A retry hint supplies duration only after the error's scope is classified.
func rateLimitHint(err error, fallback time.Duration) time.Duration {
	var hint retryAfterError
	if stderrors.As(err, &hint) && hint.RetryAfter() > 0 {
		return BoundRateLimitCooldown(hint.RetryAfter())
	}
	return fallback
}

// creditExhaustionVerdict maps a spent balance onto the capability state of the
// channels that have one. They keep such an account in the pool for their free
// tier instead of parking it outright, which is exactly why the distinction from
// a plain rate limit matters: a plain 429 is re-admitted as fully capable as soon
// as its cooldown elapses, so metered traffic reaches an account with nothing left
// to spend.
func creditExhaustionVerdict(acc *store.Account, message string, err error, at time.Time) (Verdict, bool) {
	switch {
	case strings.EqualFold(strings.TrimSpace(accountType(acc)), "workbuddy"):
		return Verdict{
			Status: store.AccountStatusWorkBuddyQuotaExhausted, Message: message,
			Scope: ScopeAccount, Retryable: Retryable(err), SwitchAccount: true, At: at,
		}, true
	case strings.EqualFold(strings.TrimSpace(accountType(acc)), "qoder"):
		return Verdict{
			Status: store.AccountStatusQoderQuotaExhausted, Message: message,
			Scope: ScopeAccount, Retryable: Retryable(err), SwitchAccount: true, At: at,
		}, true
	default:
		return Verdict{}, false
	}
}

// isClientRefusal reports a failure the account cannot influence, because it is
// a verdict about the request: the safety review rejected the input, or the
// upstream named a bad parameter.
//
// It must stay in step with the classifier in internal/errors: the same text
// has to mean the same thing whether the request path is asking "retry?" or the
// account policy is asking "is this account still usable?".
func isClientRefusal(lower string) bool {
	return strings.Contains(lower, "content policy rejected") ||
		strings.Contains(lower, "datainspectionfailed") ||
		strings.Contains(lower, "input text data may contain") ||
		strings.Contains(lower, "rejected the request parameters")
}

// isGlobalUpstreamRefusal reports whether the upstream refused because a shared
// resource is unavailable, so the refusal is identical for every account and
// rotation cannot help.
//
// The markers are deliberately about the *shape* of the refusal rather than one
// phrasing, because the same condition reaches here under different text:
//
//   - "qoder gateway is busy"    -- the classified form
//   - code 10605 / isQueued      -- Qoder's queue/busy business code, which also
//     arrives wrapped in a 401 envelope that a parser can misread as a
//     credential rejection ("qoder upstream rejected the credential: {...}")
//   - serviceAvailable:false     -- the upstream stating the service itself is
//     down for the model
//   - "available upstream accounts are rate-limited" -- the upstream saying its
//     own account pool is throttled, not ours
//
// The quoted markers are matched against the text with JSON escaping removed.
// Qoder's payload reaches us as a JSON string nested inside another one, so the
// haystack holds `\"isqueued\":true`; searching it for `"isqueued":true` never
// matched, and the classification survived only because the bare "10605" digits
// happened to be in the same string.
func isGlobalUpstreamRefusal(text string) bool {
	text = strings.ReplaceAll(strings.ToLower(text), `\`, "")
	return apperrors.IsQoderGatewayBusy(text) ||
		strings.Contains(text, "available upstream accounts are rate-limited") ||
		strings.Contains(text, "available upstream accounts are rate limited") ||
		strings.Contains(text, "10605") ||
		strings.Contains(text, `"serviceavailable":false`) ||
		strings.Contains(text, `"isqueued":true`)
}

// isEntitlementRefusal reports a model-scoped refusal that no amount of waiting
// changes, because the account's plan does not include the model. The pool stores
// this beside the cooldown so an emptied pool can be answered as "not available
// here" rather than as "temporarily rate-limited".
func isEntitlementRefusal(lower string) bool {
	return strings.Contains(lower, "no usable plan or allowance") ||
		strings.Contains(lower, "not subscribed to required model plan") ||
		apperrors.IsClineProductSurface(lower) ||
		(strings.Contains(lower, "http 403") && strings.Contains(lower, "entitlement"))
}

// modelCooldownKindFor labels a model-scoped cooldown with what it means. A
// missing label would leave the pool guessing from the deadline alone, so every
// model-scoped verdict is labelled here.
func modelCooldownKindFor(lower string) store.ModelCooldownReason {
	if isEntitlementRefusal(lower) {
		return store.ModelCooldownUnavailable
	}
	return store.ModelCooldownThrottled
}

// isModelScopedFailure reports whether the message blames a model rather than
// the credential or the account's quota.
func isModelScopedFailure(lower string) bool {
	return strings.Contains(lower, "code=6004") ||
		apperrors.IsQoderGatewayBusy(lower) ||
		apperrors.IsQoderAgentLimit(lower) ||
		apperrors.IsQoderModelRateLimited(lower) ||
		strings.Contains(lower, "available upstream accounts are rate-limited") ||
		strings.Contains(lower, "agentlimitresettime") ||
		strings.Contains(lower, "model is not found") ||
		strings.Contains(lower, "model not found") ||
		strings.Contains(lower, "no_implementation_available") ||
		strings.Contains(lower, "context_window_exceeded") ||
		strings.Contains(lower, "max_token_limit") ||
		strings.Contains(lower, "model unavailable") ||
		strings.Contains(lower, "not subscribed to required model plan") ||
		apperrors.IsClineProductSurface(lower) ||
		(strings.Contains(lower, "http 403") && strings.Contains(lower, "entitlement")) ||
		strings.Contains(lower, "requested base model")
}

// credentialMessage explains a refused credential in operator terms. The action
// must be concrete: waiting cannot repair a credential the upstream retired.
func credentialMessage(acc *store.Account, fallback string) string {
	switch {
	case isGrok(acc):
		return "上游拒绝该 Grok Build OAuth 授权，请在账号管理中重新完成官方登录"
	case acc != nil && strings.EqualFold(strings.TrimSpace(acc.AccountType), "workbuddy"):
		return "上游拒绝该 WorkBuddy 授权，请在账号管理中重新完成 OAuth 登录"
	case acc != nil && strings.EqualFold(strings.TrimSpace(acc.AccountType), "cline"):
		return "上游拒绝该 Cline 授权，请在账号管理中重新完成 OAuth 登录"
	default:
		return "上游拒绝该凭据，需要重新登录后重试"
	}
}

func isGrok(acc *store.Account) bool {
	return acc != nil && strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok")
}

func accountType(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	return acc.AccountType
}

// AccountHeld reports whether the account status is still within its cooldown.
// It is the one place the pool and the scheduler ask "may I use this account?".
func AccountHeld(acc *store.Account, now time.Time) bool {
	if acc == nil {
		return false
	}
	if !store.AccountAuthActive(acc) {
		return true
	}
	status := strings.TrimSpace(acc.StatusCode)
	if status == "" || status == store.AccountStatusQoderQuotaExhausted || status == store.AccountStatusWorkBuddyQuotaExhausted {
		return false
	}
	if acc.LastAttempt.IsZero() {
		return true
	}
	until := acc.LastAttempt.Add(CooldownFor(acc))
	// QuotaResetAt is an explicit deadline somebody recorded: the rate-limit
	// reset for a 429/402, or the short server-fault hold the gateway applies
	// after a 5xx. It is never allowed to shorten a definitive block (401/403).
	//
	// A 429 is the one status whose recorded reset cannot be taken at face value
	// as its hold length. Providers leave their billing-cycle end in the same
	// field a throttle leaves its reset in, and WorkBuddy's quota sync does
	// exactly that: a free-plan cycle end days away. Honouring it unclamped held
	// every WorkBuddy account until the cycle boundary after a single one-minute
	// throttle — the pool emptied, the channel answered 503, and the log showed
	// only a handful of 429s. A rate limit is a short capacity problem, so its
	// extension is capped at the same ceiling RateLimitCooldown itself uses; a
	// genuine retry-after shorter than that still wins.
	if acc.QuotaResetAt.After(until) {
		switch {
		case status == "429":
			// Cline's inference cap is an explicit account window (often many
			// hours), not an ordinary throttle. Preserve the upstream deadline;
			// the generic 30m ceiling only guards billing-cycle timestamps that
			// accidentally leak into normal rate-limit state.
			if apperrors.IsClineInferenceCap(strings.ToLower(acc.StatusMessage)) ||
				(strings.EqualFold(acc.AccountType, "qoder") && strings.Contains(strings.ToLower(acc.StatusMessage), "billing daily count exceeded")) {
				until = acc.QuotaResetAt
			} else if ceiling := acc.LastAttempt.Add(CooldownRateLimitMax); ceiling.Before(acc.QuotaResetAt) {
				until = ceiling
			} else {
				until = acc.QuotaResetAt
			}
		case status == "402", !strings.HasPrefix(status, "4"):
			until = acc.QuotaResetAt
		}
	}
	return now.Before(until)
}

// CooldownFor returns the cooldown the account's current status implies.
func CooldownFor(acc *store.Account) time.Duration {
	if acc == nil {
		return CooldownTransient
	}
	switch strings.TrimSpace(acc.StatusCode) {
	case "401":
		return CredentialReverify
	case "429":
		return RateLimitCooldown(acc.RateLimitFailures)
	case "402":
		// WorkBuddy reaches this with a status only when its allowance is gone: a
		// model-scoped refusal writes no status at all (see Classify), so there is
		// nothing here to release early. The account is held for the payment
		// cooldown, and isAccountAvailable releases it sooner when QuotaResetAt
		// says the allowance is back.
		return CooldownPayment
	case "403", "404":
		if isGrok(acc) {
			return CooldownBlockedGro
		}
		return CooldownBlocked
	default:
		return CooldownTransient
	}
}

// RateLimitCooldown returns 30s, 1m, 2m ... capped at 30m. A missing legacy
// failure count is treated as the first failure.
func RateLimitCooldown(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	cooldown := CooldownRateLimitBase
	for i := 1; i < failures && cooldown < CooldownRateLimitMax; i++ {
		cooldown *= 2
	}
	if cooldown > CooldownRateLimitMax {
		return CooldownRateLimitMax
	}
	return cooldown
}

// BoundRateLimitCooldown applies the configured policy ceiling to an upstream
// Retry-After/reset duration.
func BoundRateLimitCooldown(retryAfter time.Duration) time.Duration {
	if retryAfter <= 0 {
		return retryAfter
	}
	if retryAfter > CooldownRateLimitMax {
		return CooldownRateLimitMax
	}
	return retryAfter
}

// NeedsReverify reports whether a credential the upstream refused is due to be
// re-asked. Until CredentialReverify has elapsed the answer is no: re-asking a
// refused credential on every tick consumes the budget healthy accounts need.
// The operator installing a new credential clears the verdict stamp, which makes
// the account due immediately.
func NeedsReverify(acc *store.Account, now time.Time) bool {
	if acc == nil || strings.TrimSpace(acc.StatusCode) != "401" {
		return false
	}
	if acc.VerifiedAt.IsZero() {
		return true
	}
	return now.Sub(acc.VerifiedAt) >= CredentialReverify
}
