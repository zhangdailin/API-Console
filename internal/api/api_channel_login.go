package api

import (
	"context"
	"net/http"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

// The browser logins of every channel are one flow:
//
//	POST   /api/{channel}/login          start an authorization transaction
//	GET    /api/{channel}/login/{id}     observe it while the operator authorizes
//	DELETE /api/{channel}/login/{id}     abandon it
//
// The channel supplies the three upstream steps through provider.Login; the
// routing, the admission limit, the poll loop and the persistence are here once.
// Only the browser-safe half of a challenge is ever serialized: the device code
// and the channel's private material stay in this process.

// browserLogin is the server-side half of an in-flight login.
type browserLogin struct {
	deviceLogin
	// challenge is the channel's transaction, including the half that must
	// never reach the browser.
	challenge provider.LoginChallenge
	// flow is captured at start, so one transaction is served by one
	// implementation for its whole life.
	flow *provider.Login
}

// newBrowserLoginRegistry builds the registry for one channel. Each channel
// keeps its own, so codes and credentials can never cross authentication flows
// and one channel's abandoned logins cannot use up another's admission limit.
func newBrowserLoginRegistry(id channel.ID) *deviceLoginRegistry[browserLogin] {
	ready := deviceLoginReady
	if flow := loginFlow(id); flow != nil && !flow.NeedsDeviceCode {
		ready = deviceLoginReadyWithoutCode
	}
	return newDeviceLoginRegistry(
		func(login *browserLogin) *deviceLogin { return &login.deviceLogin },
		ready, channelLabel(id)+" authorization expired")
}

// HandleWorkBuddyLogin starts and observes the official WorkBuddy browser
// authorization flow. It is registered behind the administrator session
// middleware.
func (a *API) HandleWorkBuddyLogin(w http.ResponseWriter, r *http.Request) {
	a.handleBrowserLogin(w, r, channel.WorkBuddy, a.workbuddyLogins)
}

// HandleQoderLogin starts and observes the official Qoder device authorization
// flow. It is registered behind the administrator session middleware.
func (a *API) HandleQoderLogin(w http.ResponseWriter, r *http.Request) {
	a.handleBrowserLogin(w, r, channel.Qoder, a.qoderLogins)
}

// HandleClineLogin starts and observes the official Cline device authorization
// flow. It is registered behind the administrator session middleware.
func (a *API) HandleClineLogin(w http.ResponseWriter, r *http.Request) {
	a.handleBrowserLogin(w, r, channel.Cline, a.clineLogins)
}

func (a *API) handleBrowserLogin(w http.ResponseWriter, r *http.Request, id channel.ID, registry *deviceLoginRegistry[browserLogin]) {
	label := channelLabel(id)
	routeBrowserLogin(w, r, "/api/"+string(id)+"/login",
		func(w http.ResponseWriter, r *http.Request) { a.startBrowserLogin(w, r, id, registry) },
		func(w http.ResponseWriter, loginID string) { respondLoginStatus(w, registry, loginID, label) },
		func(w http.ResponseWriter, loginID string) { abandonLogin(w, registry, loginID, label) })
}

func (a *API) startBrowserLogin(w http.ResponseWriter, r *http.Request, id channel.ID, registry *deviceLoginRegistry[browserLogin]) {
	label := channelLabel(id)
	enabled, ok := beginBrowserLogin(w, r, a, label, registry)
	if !ok {
		return
	}
	flow := loginFlow(id)
	if flow == nil || flow.Start == nil {
		writeLoginError(w, http.StatusNotFound, "login_unsupported", label+" has no browser login")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cfg := a.config.Load()
	challenge, err := flow.Start(ctx, cfg)
	if err != nil {
		writeAuthorizationStartFailure(w, err, authorizationFailure{
			channel:           label,
			unreachableDetail: flow.UnreachableDetail,
			unavailable:       flow.ErrUnavailable,
			rejected:          flow.ErrRejected,
		})
		return
	}

	admitBrowserLogin(w, registry, label, loginSeed{
		// The device code lives in the shared state because the registry's
		// readiness test may require it, and the polling response never
		// serializes it: deviceLoginResponse carries only what the browser
		// needs, so the exchangeable half cannot reach the console.
		deviceCode: challenge.DeviceCode,
		userCode:   challenge.UserCode,
		verifyURI:  challenge.VerifyURI,
		verifyFull: challenge.VerifyFull,
		expiresAt:  challenge.ExpiresAt,
		interval:   challenge.Interval,
		message:    "Waiting for " + label + " authorization",
		enabled:    enabled,
		cfg:        cfg,
	}, func(shared deviceLogin) *browserLogin {
		return &browserLogin{deviceLogin: shared, challenge: challenge, flow: flow}
	}, func(pollCtx context.Context, loginID string) {
		a.pollChannelLogin(pollCtx, loginID, label, registry)
	})
}

// pollChannelLogin exchanges the challenge until the browser step completes,
// then builds and persists the account. The configuration is the snapshot taken
// at start: a live reload must not start a login on one host and poll another.
func (a *API) pollChannelLogin(ctx context.Context, loginID, label string, registry *deviceLoginRegistry[browserLogin]) {
	login, ok := registry.pollable(loginID)
	if !ok {
		return
	}
	flow, cfg := login.flow, login.configSnapshot
	pollBrowserLogin(a, ctx, registry, loginID, label, flow.ErrPending, flow.BuildVerifies,
		func(reqCtx context.Context, login *browserLogin) (provider.LoginGrant, error) {
			return flow.Exchange(reqCtx, cfg, login.challenge)
		},
		func(login *browserLogin, grant provider.LoginGrant) (*store.Account, error) {
			return flow.Build(ctx, cfg, loginID, login.challenge, grant)
		}, flow.MarkReplace)
}
