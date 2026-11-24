package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/provider"
	"orchids-api/internal/util"
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

func (l *deviceLogin) loginPayload() (string, string, time.Time) {
	if updater, ok := l.challenge.Private.(provider.LoginPayloadUpdater); ok {
		return updater.LoginPayload()
	}
	return l.challenge.QRCode, l.challenge.VerifyFull, l.challenge.ExpiresAt
}

// HandleWorkBuddyLogin starts and observes the official WorkBuddy browser
// authorization flow. It is registered behind the administrator session
// middleware.
func (a *API) HandleWorkBuddyLogin(w http.ResponseWriter, r *http.Request) {
	a.handleBrowserLogin(w, r, channel.WorkBuddy, a.workbuddyLogins)
}

// HandleClineLogin starts and observes the official Cline device authorization
// flow. It is registered behind the administrator session middleware.
func (a *API) HandleClineLogin(w http.ResponseWriter, r *http.Request) {
	a.handleBrowserLogin(w, r, channel.Cline, a.clineLogins)
}

// HandleDoubaoLogin runs the Doubao App QR login. The QR target is browser-safe
// and returned as a PNG data URL; cookies and passport state remain server-side.
func (a *API) HandleDoubaoLogin(w http.ResponseWriter, r *http.Request) {
	a.handleBrowserLogin(w, r, channel.Doubao, a.doubaoLogins)
}

// HandleWarpLogin uses the shared server-owned official device transaction.
func (a *API) HandleWarpLogin(w http.ResponseWriter, r *http.Request) {
	a.handleBrowserLogin(w, r, channel.Warp, a.warpLogins)
}

func (a *API) handleBrowserLogin(w http.ResponseWriter, r *http.Request, id channel.ID, registry *deviceLoginRegistry) {
	label := channelLabel(id)
	routeBrowserLogin(w, r, "/api/"+string(id)+"/login",
		func(w http.ResponseWriter, r *http.Request) { a.startBrowserLogin(w, r, id, registry) },
		func(w http.ResponseWriter, loginID string) { respondLoginStatus(w, registry, loginID, label) },
		func(w http.ResponseWriter, loginID string) { abandonLogin(w, registry, loginID, label) })
}

func (a *API) startBrowserLogin(w http.ResponseWriter, r *http.Request, id channel.ID, registry *deviceLoginRegistry) {
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

	login := newDeviceLogin(cfg, flow, challenge)
	login.message = "Waiting for " + label + " authorization"
	login.enabled = enabled
	response, err := registry.start(login, func(ctx context.Context, loginID string) {
		a.pollChannelLogin(ctx, loginID, label, registry)
	})
	if errors.Is(err, errDeviceLoginLimit) {
		writeLoginError(w, http.StatusTooManyRequests, "too_many_logins", "too many pending "+label+" logins; finish or cancel one first")
		return
	}
	if err != nil {
		writeLoginError(w, http.StatusInternalServerError, "transaction_failed", "failed to create login transaction")
		return
	}
	util.WriteJSON(w, response)
}
