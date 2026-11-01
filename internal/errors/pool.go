package errors

import "strings"

// The client-facing text for a request the account pool cannot take. They are
// constants because the same condition must read the same way wherever it is
// reported — the generic session handler and every provider handler answer the
// same pool — and because the pool's own note ("all matching accounts are
// cooling down for the requested model") is a diagnostic that names internal
// state: it belongs in the log, not in a body a client may show to a user.
const (
	// PoolAllowanceMessage answers an exhausted allowance: waiting is not enough,
	// somebody has to add credits or capacity.
	PoolAllowanceMessage = "Request failed: every account for this channel has exhausted its allowance. Add credits or accounts, or wait for the quota reset."
	// PoolModelCooldownMessage is the channel-neutral form of the same answer.
	PoolModelCooldownMessage = "Request failed: the requested model is cooling down on this channel. Please retry after its cooldown or choose another model."
	// PoolClineInferenceCapMessage answers Cline's inference cap, which holds
	// the whole account for the duration the upstream stated rather than one
	// model, so the answer names the account and says how long.
	PoolClineInferenceCapMessage = "Request failed: this Cline account reached its inference cap and is cooling down. Please wait for the cap window to pass or add another Cline account."
	// PoolRateLimitedMessage answers a pool every account of which is cooling down.
	PoolRateLimitedMessage = "Request failed: all available accounts for this channel are currently rate-limited. Please wait for cooldown or add another valid account."
	// PoolBusyMessage answers a pool whose accounts are all serving other requests.
	PoolBusyMessage = "Request failed: every account for this channel is busy with other requests. Please retry shortly."
	// PoolModelUnavailableMessage answers a request for a model the channel's
	// accounts cannot route (per-account model choices).
	PoolModelUnavailableMessage = "Request failed: the requested model is not available on this channel's accounts. Choose another model or add an account that supports it."
	// PoolNoAccountsMessage answers the residual cases (no accounts, or accounts
	// that cannot serve at all). The client can do nothing about them, so the
	// answer points at the operator instead of at the request.
	PoolNoAccountsMessage = "Request failed: no account in this channel can serve the request. Please check the account pool in Admin UI or add valid accounts."
	// PoolRetriesExhaustedMessage is the mid-request default: attempts were made
	// and every one of them failed.
	PoolRetriesExhaustedMessage = "Request failed: retries exhausted and no available accounts. Please check account statuses in Admin UI or add valid accounts."
)

// PoolExhaustion is how a request that no account could take is answered.
//
// Category drives both the status and the error type, through StatusForCategory,
// so the status a client sees and the text it reads cannot disagree. An empty
// Category means "nothing specific could be said": the caller supplies the
// default for its entrance.
type PoolExhaustion struct {
	Category string
	Message  string
}

// Empty reports whether the classifier recognised the cause. Callers use it to
// decide between the classified answer and their own default.
func (p PoolExhaustion) Empty() bool { return strings.TrimSpace(p.Category) == "" }

// ClassifyPoolExhaustion maps "no account in this channel could take this
// request" onto the answer the client gets.
//
// Every provider handler and the generic session handler share it, because they
// used to disagree about the same condition: one entrance answered a cooling or
// exhausted pool with a retryable 429, while another answered it with 503
// "overloaded_error" carrying the pool's internal note — so a capacity problem
// reached the caller as a server fault. selectErr is the pool's own error (its
// parenthetical says why the pool is empty); lastErr is an upstream error when
// there is one, which is the more specific answer when it is present.
func ClassifyPoolExhaustion(selectErr error, lastErr string) PoolExhaustion {
	lowerLastErr := strings.ToLower(strings.TrimSpace(lastErr))
	lowerSelect := ""
	if selectErr != nil {
		lowerSelect = strings.ToLower(selectErr.Error())
	}
	switch {
	case IsWorkBuddyRestricted(lowerLastErr) || IsWorkBuddyRestricted(lowerSelect):
		return PoolExhaustion{Category: "account_restricted", Message: CategoryMessage("account_restricted")}
	case IsCreditExhaustion(lowerLastErr) || strings.Contains(lowerSelect, "exhausted their allowance"):
		return PoolExhaustion{Category: "quota_exhausted", Message: PoolAllowanceMessage}
	// A plan/entitlement refusal is not a rate limit: the credential works and
	// the account simply does not cover the model. Answering it with "retry
	// after the cooldown" told clients to retry a condition that only a plan
	// change on the upstream side can fix.
	case strings.Contains(lowerLastErr, "not subscribed to required model plan"):
		return PoolExhaustion{Category: "model_unavailable", Message: PoolModelUnavailableMessage}
	case IsClineInferenceCap(lowerLastErr):
		// The inference cap holds the whole account for the duration the
		// upstream stated, not one model, so it gets its own answer.
		return PoolExhaustion{Category: "rate_limit", Message: PoolClineInferenceCapMessage}
	case strings.Contains(lowerLastErr, "model cooldown") ||
		strings.Contains(lowerSelect, "cooling down for the requested model"):
		return PoolExhaustion{Category: "rate_limit", Message: PoolModelCooldownMessage}
	case strings.Contains(lowerSelect, "not covered by any matching account's plan"):
		// Every account of this channel carries a model cooldown, and every one of
		// those cooldowns is a plan verdict rather than a throttle. Waiting cannot
		// change a plan, so the client is told the model is unavailable here
		// instead of being invited to retry it.
		return PoolExhaustion{Category: "model_unavailable", Message: PoolModelUnavailableMessage}
	case strings.Contains(lowerSelect, "cooling down on some matching accounts"):
		// Part of the pool only needs a wait, so a retry can still work — and
		// another model always does.
		return PoolExhaustion{Category: "rate_limit", Message: PoolModelCooldownMessage}
	case strings.Contains(lowerSelect, "rate-limited or cooling down"):
		return PoolExhaustion{Category: "rate_limit", Message: PoolRateLimitedMessage}
	case strings.Contains(lowerSelect, "concurrency limit"):
		return PoolExhaustion{Category: "rate_limit", Message: PoolBusyMessage}
	case strings.Contains(lowerSelect, "is not available in the current") && strings.Contains(lowerSelect, "account pool"):
		return PoolExhaustion{Category: "model_unavailable", Message: PoolModelUnavailableMessage}
	case ClassifyUpstreamError(lastErr).Category == "rate_limit":
		return PoolExhaustion{Category: "rate_limit", Message: PoolRateLimitedMessage}
	}
	return PoolExhaustion{}
}
