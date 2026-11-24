package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/util"
)

const maxDeviceLogins = 10

type deviceLogin struct {
	challenge  provider.LoginChallenge
	flow       *provider.Login
	verifyURI  string
	verifyFull string
	expiresAt  time.Time
	interval   time.Duration
	qrCode     string
	cancel     context.CancelFunc
	done       chan struct{}
	// configSnapshot pins provider endpoints for the whole transaction. A
	// live config reload must not start a login on one host and poll another.
	configSnapshot *config.Config

	status    string
	message   string
	accountID int64

	enabled bool
}

func newDeviceLogin(cfg *config.Config, flow *provider.Login, challenge provider.LoginChallenge) *deviceLogin {
	return &deviceLogin{
		challenge: challenge, flow: flow, configSnapshot: cfg,
		verifyURI: challenge.VerifyURI, verifyFull: challenge.VerifyFull,
		expiresAt: challenge.ExpiresAt, interval: challenge.Interval, qrCode: challenge.QRCode,
		status: "pending", enabled: true,
	}
}

type deviceLoginResponse struct {
	ID                      string `json:"id"`
	Status                  string `json:"status"`
	UserCode                string `json:"user_code,omitempty"`
	VerificationURI         string `json:"verification_uri,omitempty"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresAt               string `json:"expires_at,omitempty"`
	QRCode                  string `json:"qr_code,omitempty"`
	AccountID               int64  `json:"account_id,omitempty"`
	Message                 string `json:"message,omitempty"`
}

func finishDeviceLogin(login *deviceLogin, status, message string, accountID int64) {
	if login == nil {
		return
	}
	login.challenge = provider.LoginChallenge{}
	login.verifyURI, login.verifyFull, login.qrCode = "", "", ""
	login.status = status
	login.message = message
	login.accountID = accountID
	if login.cancel != nil {
		login.cancel()
	}
}

func newDeviceLoginResponse(id string, login *deviceLogin) deviceLoginResponse {
	response := deviceLoginResponse{ID: id}
	if login == nil {
		return response
	}
	response.Status = login.status
	response.Message = login.message
	response.AccountID = login.accountID
	if login.status == "pending" {
		response.UserCode = login.challenge.UserCode
		response.VerificationURI = login.verifyURI
		response.VerificationURIComplete = login.verifyFull
		response.ExpiresAt = login.expiresAt.UTC().Format(time.RFC3339)
		response.QRCode = login.qrCode
	}
	return response
}

func newDeviceLoginID() (string, error) {
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// HandleGrokDeviceAuthorization starts and observes the official xAI Grok
// Build CLI device-authorization flow. It accepts no files, browser cookies,
// passwords, or user-supplied tokens; device codes remain server-side only.
func (a *API) HandleGrokDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	// Same route budget as the browser logins: POST starts, GET on an id
	// observes, DELETE on an id abandons. The dispatch is spelled out here
	// rather than through routeBrowserLogin only because this flow keeps its
	// own lowercase 405 body, which a client may already match on.
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/grok/device-auth"), "/")
	switch {
	case r.Method == http.MethodPost && path == "":
		a.startGrokDeviceAuthorization(w, r)
	case r.Method == http.MethodGet && path != "":
		a.getGrokDeviceAuthorization(w, path)
	case r.Method == http.MethodDelete && path != "":
		a.cancelGrokDeviceAuthorization(w, path)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) startGrokDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.store == nil {
		http.Error(w, "account store is not configured", http.StatusServiceUnavailable)
		return
	}
	a.grokLogins.cleanup(time.Now())
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	flow := loginFlow(channel.Grok)
	if flow == nil || flow.Start == nil {
		http.Error(w, "Grok device authorization is not available", http.StatusNotFound)
		return
	}
	cfg := a.config.Load()
	challenge, err := flow.Start(ctx, cfg)
	if err != nil {
		slog.Warn("Grok device authorization could not be started", "error", err)
		http.Error(w, "failed to start Grok device authorization", http.StatusBadGateway)
		return
	}
	login := newDeviceLogin(cfg, flow, challenge)
	response, err := a.grokLogins.start(login, a.pollGrokDeviceAuthorization)
	if errors.Is(err, errDeviceLoginLimit) {
		http.Error(w, "too many pending Grok device logins", http.StatusTooManyRequests)
		return
	}
	if err != nil {
		http.Error(w, "failed to create login transaction", http.StatusInternalServerError)
		return
	}
	util.WriteJSON(w, response)
}

func (a *API) getGrokDeviceAuthorization(w http.ResponseWriter, id string) {
	a.grokLogins.cleanup(time.Now())
	response, ok := a.grokLogins.response(id)
	if !ok {
		http.Error(w, "Grok device login not found", http.StatusNotFound)
		return
	}
	util.WriteJSON(w, response)
}

func (a *API) cancelGrokDeviceAuthorization(w http.ResponseWriter, id string) {
	if _, ok := a.grokLogins.cancel(id, "Grok authorization cancelled"); !ok {
		http.Error(w, "Grok device login not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) pollGrokDeviceAuthorization(ctx context.Context, id string) {
	a.pollChannelLogin(ctx, id, "Grok", a.grokLogins)
}
