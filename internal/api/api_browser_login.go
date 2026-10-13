package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"orchids-api/internal/util"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// Shared plumbing for the browser/device login flows.
//
// Every channel authenticates by opening an official page and polling for the
// result, and exposes the console the same three endpoints: POST to start, GET
// to observe, DELETE to abandon. The routing, the error envelope, the optional
// start body, the origin check and the cancellation handshake are therefore
// identical and live here. What differs is the upstream exchange, which each
// channel supplies through provider.Login.

// authorizationFailure describes how one channel's start failures are reported.
type authorizationFailure struct {
	// channel names the flow in the operator-facing messages.
	channel string
	// unreachableDetail is what the operator is told when the endpoint cannot be
	// reached. It names the host, because that is the one thing they can check.
	unreachableDetail string
	unavailable       error
	rejected          error
}

// writeAuthorizationStartFailure maps a failed start onto the console's response.
//
// Only the *classified* cause reaches the browser. An upstream failure can echo
// request material, so the detail goes to the server log and the response says
// only what the operator can act on. Keeping the three-way classification in one
// place is what makes that guarantee checkable: no channel can decide for itself
// to pass an upstream error through.
func writeAuthorizationStartFailure(w http.ResponseWriter, err error, f authorizationFailure) {
	switch {
	case errors.Is(err, f.unavailable):
		slog.Warn(f.channel+" authorization endpoint is unreachable", "error", err)
		writeLoginError(w, http.StatusBadGateway, "upstream_unreachable", f.unreachableDetail)
	case errors.Is(err, f.rejected):
		slog.Warn(f.channel+" authorization was rejected by the upstream", "error", err)
		writeLoginError(w, http.StatusBadGateway, "upstream_rejected",
			f.channel+" refused to start a login transaction")
	default:
		slog.Warn(f.channel+" authorization could not be started", "error", err)
		writeLoginError(w, http.StatusBadGateway, "upstream_error",
			"failed to start "+f.channel+" authorization")
	}
}

// beginBrowserLogin runs the checks and parsing every browser login shares
// before it contacts the upstream, and reports the operator's intended enabled
// state. It writes the rejection itself.
//
// Both checks are security-relevant and belong to the flow rather than to any
// channel: an unconfigured store cannot persist a credential, and a login started
// from another origin is a cross-site request the console must not honour.
func beginBrowserLogin[T any](w http.ResponseWriter, r *http.Request, a *API, channel string, registry *deviceLoginRegistry[T]) (enabled, ok bool) {
	if a == nil || a.store == nil {
		writeLoginError(w, http.StatusServiceUnavailable, "store_unavailable",
			"account store is not configured")
		return false, false
	}
	if !sameOriginAdminRequest(w, r, channel) {
		return false, false
	}
	options, parsed := parseLoginOptions(w, r)
	if !parsed {
		return false, false
	}
	enabled = true
	if options.Enabled != nil {
		enabled = *options.Enabled
	}
	// Drop finished transactions first, so a page of abandoned logins cannot
	// hold the admission limit against this one.
	registry.cleanup(time.Now())
	return enabled, true
}

// loginSeed is what a channel learned from its upstream about the transaction it
// just opened, together with the operator's intent for the resulting account.
type loginSeed struct {
	// deviceCode is the value the poll call exchanges. Qoder has none at this
	// point: its upstream issues one during the first poll.
	deviceCode string
	// verifyURI is the human-facing page and verifyFull the single-use challenge
	// embedded in it. A channel with no separate page leaves verifyURI empty.
	verifyURI  string
	verifyFull string
	// userCode is the short code the operator confirms on that page. It is safe
	// to show: it cannot be exchanged without the device code.
	userCode  string
	expiresAt time.Time
	interval  time.Duration
	// message is the console's "waiting" line while the browser step is pending.
	message string
	enabled bool
	cfg     *config.Config
}

// admitBrowserLogin registers a transaction and starts its poll loop.
//
// It owns the lifecycle both channels shared: allocate an id, derive the poll
// context and its cancel, build the channel's transaction around the common
// state, admit it under the limit, and start the loop that the cancel path waits
// on. attach embeds the shared state into the channel's own type; poll is that
// channel's loop.
func admitBrowserLogin[T any](
	w http.ResponseWriter,
	registry *deviceLoginRegistry[T],
	channel string,
	seed loginSeed,
	attach func(deviceLogin) *T,
	poll func(context.Context, string),
) bool {
	id, err := newDeviceLoginID()
	if err != nil {
		writeLoginError(w, http.StatusInternalServerError, "transaction_failed",
			"failed to create login transaction")
		return false
	}
	pollContext, pollCancel := context.WithCancel(context.Background())
	login := attach(deviceLogin{
		deviceCode:     seed.deviceCode,
		userCode:       seed.userCode,
		verifyURI:      seed.verifyURI,
		verifyFull:     seed.verifyFull,
		expiresAt:      seed.expiresAt,
		interval:       seed.interval,
		cancel:         pollCancel,
		done:           make(chan struct{}),
		configSnapshot: seed.cfg,
		status:         "pending",
		message:        seed.message,
		enabled:        seed.enabled,
		// The caller always declares the intended state, so the completed
		// account must honour it instead of defaulting to enabled.
		enabledKnown: true,
	})

	if !registry.admit(id, login) {
		pollCancel()
		writeLoginError(w, http.StatusTooManyRequests, "too_many_logins",
			"too many pending "+channel+" logins; finish or cancel one first")
		return false
	}

	go func() {
		defer close(registry.shared(login).done)
		poll(pollContext, id)
	}()

	util.WriteJSON(w, newDeviceLoginResponse(id, registry.shared(login)))
	return true
}

// awaitLoginPoll waits for the next poll tick and returns the transaction to
// poll.
//
// It reports false when the loop must stop: the transaction is gone, the caller
// cancelled it, or it outlived its expiry — in which case the transaction is
// finished with the channel's timeout reason so the console stops saying
// "waiting" for a login nobody is going to complete.
func awaitLoginPoll[T any](ctx context.Context, registry *deviceLoginRegistry[T], id, channel string) (*T, bool) {
	login, ok := registry.pollable(id)
	if !ok {
		return nil, false
	}
	shared := registry.shared(login)
	if time.Now().After(shared.expiresAt) {
		registry.finish(id, "expired", channel+" authorization timed out; start again", 0)
		return nil, false
	}
	select {
	case <-ctx.Done():
		return nil, false
	case <-time.After(shared.interval):
	}
	return login, true
}

// pollBrowserLogin owns the exchange/verify/persist state machine. Provider
// adapters keep private grants out of the response and preserve their builders.
//
// buildVerifies says that building the account already proved the credential,
// so a failed build is reported as a failed verification rather than a failed
// save.
func pollBrowserLogin[T, C any](a *API, ctx context.Context, registry *deviceLoginRegistry[T], id, channel string, pending error, buildVerifies bool,
	exchange func(context.Context, *T) (C, error), build func(*T, C) (*store.Account, error), markReplace func(*store.Account)) {
	for {
		login, ok := awaitLoginPoll(ctx, registry, id, channel)
		if !ok {
			return
		}
		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		creds, err := exchange(reqCtx, login)
		cancel()
		if err != nil {
			if errors.Is(err, pending) {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			slog.Warn(channel+" authorization failed", "login_id", id, "error", err)
			registry.finish(id, "failed", channel+" authorization failed; start again", 0)
			return
		}
		account, err := build(login, creds)
		if err != nil {
			logMessage := channel + " authorization succeeded but the account could not be stored"
			message := channel + " authorization succeeded but the account could not be saved: " + truncateLoginReason(err)
			if buildVerifies {
				logMessage = channel + " authorization succeeded but verification failed"
				message = channel + " authorization succeeded but the account could not be verified"
			}
			slog.Warn(logMessage, "login_id", id, "error", err)
			registry.finish(id, "failed", message, 0)
			return
		}
		if ctx.Err() != nil || !registry.pending(id) {
			return
		}
		shared := registry.shared(login)
		if shared.enabledKnown {
			account.Enabled = shared.enabled
		}
		finishBrowserLogin(a, ctx, registry, id, channel, account, markReplace)
		return
	}
}

// finishBrowserLogin persists the account a completed browser login produced.
//
// A channel may hand out a second grant for an account this gateway already has:
// the operator signs in again, or an earlier transaction completed after the
// console stopped watching it. Storing that as a new row would give the scheduler
// two rows for one allowance, so a row that already carries the credential is
// updated in place, and the operator is told whether the login added an account
// or refreshed one.
//
// markReplace is the single thing each channel must say for itself: which field
// tells the edit path that the credential just submitted supersedes the stored
// one.
func finishBrowserLogin[T any](
	a *API,
	ctx context.Context,
	registry *deviceLoginRegistry[T],
	id, channel string,
	account *store.Account,
	markReplace func(*store.Account),
) {
	existing, err := a.findDuplicateAccountByCredential(ctx, account, 0)
	if err != nil {
		registry.finish(id, "failed", channel+" authorization succeeded but the account could not be saved", 0)
		return
	}
	if existing != nil {
		account.ID = existing.ID
		if markReplace != nil {
			markReplace(account)
		}
		if err := a.store.UpdateAccount(ctx, account); err != nil {
			registry.finish(id, "failed", channel+" authorization succeeded but the account could not be updated", 0)
			return
		}
		registry.finish(id, "complete", channel+" account credentials refreshed", account.ID)
		return
	}
	if err := a.store.CreateAccount(ctx, account); err != nil {
		registry.finish(id, "failed", channel+" authorization succeeded but the account could not be saved", 0)
		return
	}
	registry.finish(id, "complete", channel+" account added", account.ID)
	a.syncAccountAfterCreate(*account)
}

// writeLoginError reports a failure with a stable machine-readable code so the
// admin UI can explain the actual cause instead of guessing. The message is
// operator-facing and never contains credentials or upstream error text.
func writeLoginError(w http.ResponseWriter, status int, code, message string) {
	util.WriteJSONStatus(w, status, map[string]string{"code": code, "error": message})
}

// loginOptions is the optional start body. Both browser logins accept the same
// single field, and nothing else is allowed through.
type loginOptions struct {
	Enabled *bool `json:"enabled"`
}

// parseLoginOptions reads the optional start body.
//
// A body is optional — the console may start a login with no payload at all —
// but an unknown field is rejected rather than ignored, because a silently
// dropped option reads to the operator as a setting that did not apply.
func parseLoginOptions(w http.ResponseWriter, r *http.Request) (loginOptions, bool) {
	var options loginOptions
	if r.Body == nil {
		return options, true
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		http.Error(w, "invalid login payload", http.StatusBadRequest)
		return options, false
	}
	if strings.TrimSpace(string(body)) == "" {
		return options, true
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&options); err != nil {
		http.Error(w, "invalid login payload", http.StatusBadRequest)
		return options, false
	}
	return options, true
}

// routeBrowserLogin dispatches the three methods of a browser login: POST to the
// bare prefix starts a transaction, GET on an id observes it, DELETE on an id
// abandons it.
func routeBrowserLogin(
	w http.ResponseWriter,
	r *http.Request,
	prefix string,
	start func(http.ResponseWriter, *http.Request),
	get func(http.ResponseWriter, string),
	cancel func(http.ResponseWriter, string),
) {
	w.Header().Set("Cache-Control", "no-store")
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/")
	switch {
	case r.Method == http.MethodPost && path == "":
		start(w, r)
	case r.Method == http.MethodGet && path != "":
		get(w, path)
	case r.Method == http.MethodDelete && path != "":
		cancel(w, path)
	default:
		writeMethodNotAllowed(w)
	}
}

// respondLoginStatus writes the current state of a login transaction.
//
// channel names the flow in the operator-facing message, and is the same string
// the start and cancel paths use, so the three endpoints cannot describe the same
// transaction differently.
func respondLoginStatus[T any](w http.ResponseWriter, registry *deviceLoginRegistry[T], id, channel string) {
	registry.cleanup(time.Now())
	response, ok := registry.response(id)
	if !ok {
		writeLoginError(w, http.StatusNotFound, "login_not_found",
			channel+" login session not found or already finished")
		return
	}
	util.WriteJSON(w, response)
}

// abandonLogin cancels a login transaction and waits for its poll loop to stop.
//
// The wait is the point: the response is the operator's confirmation that the
// transaction is gone, and a poll loop still running could otherwise persist an
// account for a login that was just abandoned.
func abandonLogin[T any](w http.ResponseWriter, registry *deviceLoginRegistry[T], id, channel string) {
	done, ok := registry.cancel(id, channel+" authorization cancelled")
	if !ok {
		writeLoginError(w, http.StatusNotFound, "login_not_found",
			channel+" login session not found or already finished")
		return
	}
	waitForLoginPoll(done)
	w.WriteHeader(http.StatusNoContent)
}

// waitForLoginPoll bounds how long a cancel waits for its poll loop. A loop that
// is inside an upstream request cannot be interrupted immediately, and the
// operator is waiting on the response, so the wait is capped rather than open.
func waitForLoginPoll(done <-chan struct{}) {
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// truncateLoginReason bounds the reason shown in the console. It is deliberately
// short: the full detail is already in the server log.
func truncateLoginReason(err error) string {
	if err == nil {
		return "unknown error"
	}
	reason := strings.TrimSpace(err.Error())
	if reason == "" {
		return "unknown error"
	}
	const limit = 200
	if len(reason) > limit {
		reason = reason[:limit] + "..."
	}
	return reason
}

// sameOriginAdminRequest enforces that a credential-mutating login attempt came
// from this admin origin over a secure context. It writes the rejection itself,
// and logs the observed origin so a reverse-proxy mismatch can be diagnosed
// from the server log instead of guessed at.
func sameOriginAdminRequest(w http.ResponseWriter, r *http.Request, provider string) bool {
	rawOrigin := r.Header.Get("Origin")
	origin, err := url.Parse(rawOrigin)
	if err != nil || origin == nil || (origin.Scheme != "http" && origin.Scheme != "https") ||
		!strings.EqualFold(origin.Host, r.Host) || origin.User != nil || origin.Path != "" ||
		origin.RawQuery != "" || origin.Fragment != "" ||
		r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		slog.Warn(provider+" login rejected: origin is not this admin origin",
			"origin", rawOrigin, "host", r.Host, "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
		writeLoginError(w, http.StatusForbidden, "origin_mismatch",
			"the login request must come from this admin page (same origin)")
		return false
	}
	if origin.Scheme != "https" && origin.Hostname() != "localhost" && origin.Hostname() != "127.0.0.1" && origin.Hostname() != "::1" {
		slog.Warn(provider+" login rejected: insecure origin", "origin", rawOrigin)
		writeLoginError(w, http.StatusForbidden, "insecure_origin",
			"HTTPS is required to sign in to "+provider+" (localhost is exempt)")
		return false
	}
	// A body is optional for the popup flow, but reject a type we do not parse
	// instead of silently ignoring whatever was sent.
	if rawType := strings.TrimSpace(r.Header.Get("Content-Type")); rawType != "" {
		if mediaType, _, _ := mime.ParseMediaType(rawType); mediaType != "application/json" {
			writeLoginError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
				"application/json is required")
			return false
		}
	}
	return true
}
