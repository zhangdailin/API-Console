package handler

import (
	"context"
	"log/slog"
	"net/http"
	"orchids-api/internal/provider"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/logutil"
	"orchids-api/internal/middleware"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

type messageExecution struct {
	h                  *Handler
	r                  *http.Request
	cfg                *config.Config
	sh                 *streamHandler
	req                ClaudeRequest
	mappedModel        string
	targetChannel      string
	forcedChannel      string
	traceID            string
	conversationKey    string
	builtPrompt        string
	effort             string
	verboseDiagnostics bool
	gateNoTools        bool
	upstreamMessages   []prompt.Message
	effectiveTools     []interface{}
	structured         *upstream.StructuredOutput
	logger             *debug.Logger
	apiClient          UpstreamClient
	currentAccount     *store.Account
	releaseClient      func()
	trackedAccountID   int64
	failedAccountIDs   []int64
	failedAccountSet   map[int64]struct{}
}

func (e *messageExecution) run() {
	// A per-request chat session id, reused across account switches so the
	// upstream keeps one conversation for this downstream request.
	chatSessionID := "chat_" + randomSessionID()
	maxRetries := e.cfg.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	retryDelay := time.Duration(e.cfg.RetryDelay) * time.Millisecond
	retriesRemaining := maxRetries
	budgetCtx, attemptBudget := upstream.WithAttemptBudget(e.r.Context(), maxRetries+1)
	e.r = e.r.WithContext(budgetCtx)
	// sharedRefusalWaited accumulates only the waits spent on a refusal that
	// every account shares, which is bounded separately from maxRetries.
	var sharedRefusalWaited time.Duration

	// Publish the model this request resolved to, so the per-minute
	// aggregation can attribute the outcome to a model rather than only to a
	// channel. The middleware cannot read the body itself.
	e.r = e.r.WithContext(middleware.WithRequestModel(e.r.Context(), e.mappedModel))

	payloadMessages := e.upstreamMessages
	// The schema instruction is prepended rather than appended: a channel
	// that truncates a long system array keeps the shape the answer must
	// have, not the caller's prose behind it.
	payloadSystem := upstream.PrependSystemHint(e.req.System, e.structured.SystemHint())

	upstreamReq := upstream.UpstreamRequest{
		ResponseFormat:    e.req.ResponseFormat,
		ResponseText:      e.req.ResponseText,
		Include:           e.req.Include,
		PromptCacheKey:    e.req.PromptCacheKey,
		ResponsesTools:    e.req.ResponsesTools,
		MaxTokens:         e.req.outputTokenLimit(),
		Temperature:       e.req.Temperature,
		TopP:              e.req.TopP,
		Stop:              e.req.stopSequences(),
		Prompt:            e.builtPrompt,
		Model:             e.mappedModel,
		Messages:          payloadMessages,
		System:            payloadSystem,
		Tools:             e.effectiveTools,
		ToolChoice:        e.req.ToolChoice,
		ParallelToolCalls: e.req.ParallelToolCalls,
		NoTools:           e.gateNoTools,
		ReasoningEffort:   e.effort,
		RequestID:         workBuddyConversationRequestID(e.r),
		ConversationID:    explicitConversationID(e.r, e.req),
		TraceID:           middleware.GetTraceID(e.r.Context()),
		ChatSessionID:     chatSessionID,
	}
	primaryHandler := func(msg upstream.SSEMessage) {
		if msg.Type == "model.text-delta" || msg.Type == "model.reasoning-delta" || msg.Type == "model.tool-call" {
			util.MarkGenerationProgress(e.r.Context())
		}
		e.sh.handleMessage(msg)
	}
	var attempt int
	for {
		if returned, _ := e.sh.terminalState(); returned {
			return
		}
		e.sh.resetRoundState()
		var err error
		upstreamReq.Attempt = attempt + 1
		accountID := int64(0)
		accountType, accountName := "", ""
		if e.currentAccount != nil {
			accountID, accountType, accountName = e.currentAccount.ID, e.currentAccount.AccountType, e.currentAccount.Name
		}
		if e.verboseDiagnostics {
			slog.Debug(
				"Calling upstream client",
				"trace_id", e.traceID,
				"attempt", upstreamReq.Attempt,
				"max_attempts", maxRetries+1,
				"channel", e.targetChannel,
				"model", e.mappedModel,
				"conversation_id", e.conversationKey,
				"chat_session_id", chatSessionID,
				"account_id", accountID,
				"account_type", accountType,
				"account_name", accountName,
			)
			slog.Debug("Using SendRequestWithPayload")
		}

		callsBefore := attemptBudget.Used()
		callCtx := upstream.WithAttemptObserver(e.r.Context(), func(failed bool) {
			middleware.RecordUpstreamAttempt(e.r.Context(), accountID, failed)
		})
		err = e.apiClient.SendRequestWithPayload(callCtx, upstreamReq, primaryHandler, e.logger)
		// The same account id the diagnostics above reported, with or without
		// diagnostics enabled.
		if attemptBudget.Used() == callsBefore {
			middleware.RecordUpstreamAttempt(e.r.Context(), accountID, err != nil)
		}
		logutil.DebugIf(e.verboseDiagnostics, "Upstream client returned", "trace_id", e.traceID, "attempt", upstreamReq.Attempt, "error", err)

		if err == nil {
			e.sh.forceFinishIfMissing()
			logutil.DebugIf(e.verboseDiagnostics, "Upstream attempt completed", "trace_id", e.traceID, "attempt", upstreamReq.Attempt)
			break
		}
		// A provider may emit its authoritative finish frame and then observe a
		// transport cleanup error. Never reset terminal state and append a second
		// response in that case.
		if returned, failed := e.sh.terminalState(); returned {
			if failed {
				return
			}
			slog.Warn("Ignoring upstream error after terminal response", "trace_id", e.traceID, "attempt", upstreamReq.Attempt, "error", err)
			break
		}
		if e.r.Context().Err() != nil {
			category := "client"
			if context.Cause(e.r.Context()) == context.DeadlineExceeded {
				category = "timeout"
			}
			e.sh.reportRequestFailure("Request deadline or cancellation", category, apperrors.PublicMessage("context deadline exceeded"), 0)
			return
		}
		errStr := err.Error()
		errClass := apperrors.ClassifyUpstreamError(errStr)
		if e.sh.hasAnyOutput() {
			slog.Warn("Upstream failed after partial output, skip retry to avoid duplicated token billing", "trace_id", e.traceID, "attempt", upstreamReq.Attempt, "error", err)
			// Partial content is not a successful completion. Streaming responses
			// have already committed 200, so report the terminal failure in band;
			// non-streaming responses have committed nothing and can still return
			// the correct HTTP error without leaking the partial draft.
			e.sh.reportRequestFailure("Reporting upstream failure after partial output",
				errClass.Category, apperrors.PublicMessage(errStr), upstreamRetryAfter(err))
			return
		}

		// Check for non-retriable errors
		slog.Error("Request error", "trace_id", e.traceID, "attempt", upstreamReq.Attempt, "error", err, "category", errClass.Category, "retryable", errClass.Retryable)
		// One decision for both questions this error raises: whether the
		// account keeps its place in the pool, and whether the request may be
		// retried. The scheduler reads the same policy, so a failure cannot be
		// "cooling down" for one entrance and "retryable" for the other.
		verdict := accountpolicy.Classify(e.currentAccount, err, e.req.Model)
		// Mark the account status (auth-class errors are always marked,
		// whether or not they are retryable)
		if e.currentAccount != nil && e.h.loadBalancer != nil && e.h.loadBalancer.Store != nil {
			if verdict.Scope == accountpolicy.ScopeModel && verdict.Model != "" && verdict.Cooldown > 0 {
				// WorkBuddy code 6004 is a model-frequency limit. Persist only
				// that model's cooldown; applying an empty account status here
				// would either be skipped or accidentally clear unrelated state.
				// The verdict's kind travels with the deadline: the selection
				// layer cannot see this request, and has to know whether the
				// model is throttled or simply not covered by the plan.
				store.RecordModelCooldownWithReason(e.currentAccount, verdict.Model, time.Now().Add(verdict.Cooldown), verdict.ModelCooldownKind)
				if persistErr := e.h.loadBalancer.Store.UpdateAccount(e.r.Context(), e.currentAccount); persistErr != nil {
					slog.Warn("persist model cooldown failed", "account_id", e.currentAccount.ID, "model", verdict.Model, "error", persistErr)
				}
			} else if verdict.Status != "" {
				logutil.DebugIf(e.verboseDiagnostics, "标记账号状态", "account_id", e.currentAccount.ID, "status", verdict.Status, "scope", string(verdict.Scope), "category", errClass.Category)
				// Apply keeps the status and its operator-facing reason
				// together, so the account table can explain the cooldown.
				verdict.Apply(e.currentAccount)
				// WorkBuddy keeps a spent account in the pool for its free tier
				// only. When the upstream refuses that tier too — production
				// answers 14018 "Credits exhausted" for a zero-balance plan on a
				// confirmed free model — the tier is not available on this account
				// either, and every later request pays for the same upstream
				// rejection before switching. Scope the verdict to the model, so
				// the pool stops offering this account for it while the balance is
				// spent; the account keeps its free-only capability state.
				if verdict.Status == store.AccountStatusWorkBuddyQuotaExhausted &&
					provider.IsFreeModel("workbuddy", e.currentAccount, upstreamReq.Model) {
					// Hold the model out of this account until its plan resets, but
					// never past the cap below: the reset comes from the upstream's
					// wall clock, whose zone the gateway cannot verify, so a misread
					// boundary must not park a model for hours longer than the
					// refusal deserves.
					const freeTierMaxHold = 6 * time.Hour
					freeTierHold := freeTierMaxHold
					if reset := e.currentAccount.WorkBuddyQuota.ResetAt; reset.After(time.Now()) {
						if until := time.Until(reset); until < freeTierHold {
							freeTierHold = until
						}
					}
					slog.Warn("WorkBuddy free tier refused on a spent account; scoping the model out",
						"account_id", e.currentAccount.ID, "model", upstreamReq.Model, "hold", freeTierHold)
					store.RecordModelCooldownWithReason(e.currentAccount, upstreamReq.Model, time.Now().Add(freeTierHold), store.ModelCooldownUnavailable)
				}
				e.h.loadBalancer.PersistAppliedAccountStatus(e.r.Context(), e.currentAccount, "账号策略判定: "+verdict.Status)
			}
		}

		if !verdict.Retryable {
			slog.Error("Aborting retries for non-retriable error", "error", err, "category", errClass.Category)
			// A failure before any output is a failure, not an answer. The
			// raw upstream text goes to the log; the client gets the category.
			if errClass.Category == "canceled" {
				if e.r.Context().Err() != nil {
					e.sh.finishResponse("end_turn")
					return
				}
				e.sh.reportRequestFailure("Reporting unexpected upstream cancellation", "server", "Upstream request was canceled unexpectedly", 0)
				return
			}
			e.sh.reportRequestFailure("Reporting non-retriable upstream failure",
				errClass.Category, apperrors.PublicMessage(errStr), upstreamRetryAfter(err))
			return
		}

		// Shared queues may clear later within the advertised retry window.
		// Use the bounded retry budget on the same account rather than handing
		// every caller an early 429 after a single short probe. Once exhausted,
		// the uncommitted response below still returns an honest HTTP 429.

		if e.r.Context().Err() != nil {
			e.sh.finishResponse("end_turn")
			return
		}
		if retriesRemaining <= 0 || attemptBudget.Remaining() <= 0 {
			if e.currentAccount != nil && e.h.loadBalancer != nil {
				slog.Error("Account request failed, max retries reached", "account", e.currentAccount.Name)
			}
			// Same rule as the non-retriable branch above: with nothing sent yet
			// this is a gateway failure, and the client sees it as one.
			e.sh.reportRequestFailure("Reporting that retries are exhausted",
				errClass.Category, apperrors.PublicMessage(errStr), upstreamRetryAfter(err))
			return
		}
		retriesRemaining--
		slog.Warn(
			"Retrying upstream request without prior output",
			"trace_id", e.traceID,
			"attempt", upstreamReq.Attempt,
			"category", errClass.Category,
			"switch_account", verdict.SwitchAccount,
			"retries_remaining", retriesRemaining,
		)
		if verdict.SwitchAccount && e.currentAccount != nil && e.h.loadBalancer != nil {
			prevClient := e.apiClient
			prevAccount := e.currentAccount
			if _, ok := e.failedAccountSet[e.currentAccount.ID]; !ok {
				e.failedAccountSet[e.currentAccount.ID] = struct{}{}
				e.failedAccountIDs = append(e.failedAccountIDs, e.currentAccount.ID)
			}
			slog.Warn("Account request failed, switching account", "account", e.currentAccount.Name, "unsuccessful_attempts", len(e.failedAccountIDs))

			// Release the old account's connection count
			if e.trackedAccountID != 0 {
				e.h.releaseTrackedAccount(e.trackedAccountID)
				e.trackedAccountID = 0
			}

			nextClient, nextAccount, releaseNext, nextTrackedAccountID, retryErr := e.h.acquireReservedAccountSelection(e.r.Context(), e.targetChannel, true, e.failedAccountIDs, accountSelectionOptions{
				ModelID: upstreamReq.Model,
			})
			if retryErr == nil {
				previousRelease := e.releaseClient
				e.apiClient = nextClient
				e.currentAccount = nextAccount
				e.releaseClient = releaseNext
				e.trackedAccountID = nextTrackedAccountID
				previousRelease()
				if e.verboseDiagnostics {
					if e.currentAccount != nil {
						slog.Debug("Switched to account", "account", e.currentAccount.Name)
					} else {
						slog.Debug("Switched to default upstream config")
					}
				}
			} else {
				if shouldRetryCurrentAccountWhenNoAlternative(errClass.Category) && prevAccount != nil {
					reacquiredID, acquired := e.h.tryAcquireTrackedAccount(prevAccount)
					if !acquired {
						slog.Error("No account concurrency slot available for retry", "account_id", prevAccount.ID, "category", errClass.Category)
						e.sh.InjectNoAvailableAccountError(errStr, retryErr)
						e.sh.finishResponse("end_turn")
						return
					}
					e.apiClient = prevClient
					e.currentAccount = prevAccount
					e.trackedAccountID = reacquiredID
					slog.Warn(
						"No alternate accounts available; retrying current account",
						"trace_id", e.traceID,
						"attempt", upstreamReq.Attempt,
						"account_id", e.currentAccount.ID,
						"category", errClass.Category,
						"retry_error", retryErr,
					)
				} else {
					slog.Error("No more accounts available", "error", retryErr)
					e.sh.InjectNoAvailableAccountError(errStr, retryErr)
					e.sh.finishResponse("end_turn")
					return
				}
			}
		}
		retryDelayForAttempt := computeRetryDelay(retryDelay, attempt+1, errClass.Category)
		if hinted := upstreamRetryAfter(err); hinted > retryDelayForAttempt {
			retryDelayForAttempt = hinted
		}
		sharedRefusal := isSharedUpstreamRefusalClass(errClass)
		// sharedRefusalWait is the selected interval, charged against the
		// budget. The jitter below is added only to the sleep: it exists to
		// decorrelate wake-ups, and charging it made the last reachable
		// window unreachable (a 30s hint plus up to 5s of jitter spent 35s
		// against a 60s budget that had already paid 30s).
		sharedRefusalWait := time.Duration(0)
		qoderIntervalMs := 0
		if e.cfg != nil && errClass.Category == "upstream_queue" {
			qoderIntervalMs = e.cfg.QoderQueueRetryIntervalMs
		}
		if retryDelayForAttempt > 0 && sharedRefusal {
			// A configured Qoder interval overrides its provider hint. Other
			// channels keep their existing early-probe policy.
			sharedRefusalWait = sharedRefusalWaitForChannel(retryDelayForAttempt, attempt+1, e.targetChannel, qoderIntervalMs)
		}
		// configSnapshot reports nil for a nil handler, so the knob is read
		// defensively: losing the setting must fall back to the built-in
		// bound, not panic inside the retry loop.
		budgetMs := 0
		if e.cfg != nil {
			budgetMs = e.cfg.SharedRefusalWaitBudgetMs
		}
		waitBudget := SharedRefusalWaitBudget(budgetMs)
		queueDisabled := false
		if e.targetChannel == "qoder" && errClass.Category == "upstream_queue" && e.cfg != nil {
			if specific := e.cfg.QoderQueueBudget(); specific >= 0 {
				waitBudget = specific
				queueDisabled = specific == 0
			}
		}
		// A shared refusal is a gate on the upstream's side, not this account's
		// throttle, so the wait is spent on the same account and can repeat.
		// Bound the total by the shortest deadline in front of this process,
		// which is the edge proxy's origin timeout, not the caller's patience:
		// answering past the edge's limit does not give the caller the work,
		// it gives it a 520 from the edge.
		if sharedRefusal && (queueDisabled || (sharedRefusalWait > 0 && !sharedRefusalWaitAllowedWithin(sharedRefusalWaited, sharedRefusalWait, waitBudget))) {
			slog.Warn("Shared upstream refusal exceeded the wait budget; answering now",
				"trace_id", e.traceID,
				"waited", sharedRefusalWaited,
				"next", sharedRefusalWait,
				"budget", waitBudget,
				"category", errClass.Category,
			)
			e.sh.reportRequestFailure("Reporting a shared refusal after the wait budget",
				errClass.Category, apperrors.PublicMessage(errStr), upstreamRetryAfter(err))
			return
		}
		if sharedRefusalWait > 0 {
			retryDelayForAttempt = sharedRefusalSleepForChannel(sharedRefusalWait, e.targetChannel, qoderIntervalMs)
		}
		// Holding this account's concurrency slot through the wait starves the
		// pool: the slot is reserved for the whole request, so ten requests
		// waiting out a gate would occupy one account completely while the rest
		// of the pool idled. Release it for the wait and take it back before the
		// next attempt; if it is gone by then the account really is busy.
		slotReleasedForWait := false
		if retryDelayForAttempt > 0 && sharedRefusal && e.trackedAccountID != 0 {
			e.h.releaseTrackedAccount(e.trackedAccountID)
			e.trackedAccountID = 0
			slotReleasedForWait = true
		}
		waitStarted := time.Now()
		waitCompleted := true
		if retryDelayForAttempt > 0 {
			waitCompleted = util.SleepWithContext(e.r.Context(), retryDelayForAttempt)
			debug.RecordWait(e.r.Context(), errClass.Category, retryDelayForAttempt, time.Since(waitStarted), !waitCompleted)
			middleware.RecordRetryWait(e.r.Context(), errClass.Category, time.Since(waitStarted))
		}
		if !waitCompleted {
			e.sh.reportRequestFailure("Request canceled during retry wait", "timeout", apperrors.PublicMessage("context deadline exceeded"), 0)
			return
		}
		if sharedRefusal {
			sharedRefusalWaited += sharedRefusalWait
			if sharedRefusalWait == 0 {
				// No hint to charge, but the attempt is still repeated: count
				// the delay actually spent so a hintless gate cannot loop
				// forever without moving the budget.
				sharedRefusalWaited += retryDelayForAttempt
			}
		}
		if slotReleasedForWait && e.currentAccount != nil {
			reacquiredID, acquired := e.h.tryAcquireTrackedAccount(e.currentAccount)
			if !acquired {
				// Another request took the slot while this one waited. Ending
				// here is the honest answer: the account is at its limit, and
				// retrying would either exceed that limit or make the caller
				// wait through a second gate.
				slog.Warn("Account became busy during a shared-refusal wait; ending the request",
					"trace_id", e.traceID, "account_id", e.currentAccount.ID, "account", e.currentAccount.Name)
				e.sh.reportRequestFailure("Reporting a busy pool after a shared-refusal wait",
					"rate_limit", apperrors.PoolBusyMessage, 0)
				return
			}
			e.trackedAccountID = reacquiredID
		}
		attempt++
	}
}
