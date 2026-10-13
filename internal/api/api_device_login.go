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
	deviceCode string
	userCode   string
	verifyURI  string
	verifyFull string
	expiresAt  time.Time
	interval   time.Duration
	cancel     context.CancelFunc
	done       chan struct{}
	// configSnapshot pins provider endpoints for the whole transaction. A
	// live config reload must not start a login on one host and poll another.
	configSnapshot *config.Config

	status    string
	message   string
	accountID int64

	// enabled/enabledKnown carry a provider-specific preference captured at
	// start time (currently the WorkBuddy account enabled flag).
	enabled      bool
	enabledKnown bool
}

type deviceLoginResponse struct {
	ID                      string `json:"id"`
	Status                  string `json:"status"`
	UserCode                string `json:"user_code,omitempty"`
	VerificationURI         string `json:"verification_uri,omitempty"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresAt               string `json:"expires_at,omitempty"`
	AccountID               int64  `json:"account_id,omitempty"`
	Message                 string `json:"message,omitempty"`
}

func finishDeviceLogin(login *deviceLogin, status, message string, accountID int64) {
	if login == nil {
		return
	}
	login.deviceCode = ""
	login.userCode = ""
	login.verifyURI = ""
	login.verifyFull = ""
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
		response.UserCode = login.userCode
		response.VerificationURI = login.verifyURI
		response.VerificationURIComplete = login.verifyFull
		response.ExpiresAt = login.expiresAt.UTC().Format(time.RFC3339)
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
	id, err := newDeviceLoginID()
	if err != nil {
		http.Error(w, "failed to create login transaction", http.StatusInternalServerError)
		return
	}
	pollContext, pollCancel := context.WithCancel(context.Background())
	login := &deviceLogin{
		deviceCode: challenge.DeviceCode,
		userCode:   challenge.UserCode,
		verifyURI:  challenge.VerifyURI,
		verifyFull: challenge.VerifyFull,
		expiresAt:  challenge.ExpiresAt,
		interval:   challenge.Interval,
		cancel:     pollCancel,
		status:     "pending",
	}
	if !a.grokLogins.admit(id, login) {
		pollCancel()
		http.Error(w, "too many pending Grok device logins", http.StatusTooManyRequests)
		return
	}
	go a.pollGrokDeviceAuthorization(pollContext, id, cfg, challenge)
	util.WriteJSON(w, newDeviceLoginResponse(id, login))
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

// pollGrokDeviceAuthorization exchanges the device code until the operator
// finishes, then stores the account. cfg and challenge are the ones the
// transaction started with: a live reload must not start a login on one host
// and poll another.
func (a *API) pollGrokDeviceAuthorization(ctx context.Context, id string, cfg *config.Config, challenge provider.LoginChallenge) {
	flow := loginFlow(channel.Grok)
	if flow == nil || flow.Exchange == nil || flow.Build == nil {
		a.grokLogins.finish(id, "failed", "Grok authorization failed", 0)
		return
	}
	for {
		login, ok := a.grokLogins.pollable(id)
		if !ok {
			return
		}
		if time.Now().After(login.expiresAt) {
			a.grokLogins.finish(id, "expired", "Grok authorization expired", 0)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(login.interval):
		}
		// The device code is read from the transaction on every poll: a finished
		// transaction has dropped it, and must not be exchanged again.
		current := challenge
		current.DeviceCode = login.deviceCode
		requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		grant, err := flow.Exchange(requestCtx, cfg, current)
		cancel()
		if err != nil {
			if flow.ErrPending != nil && errors.Is(err, flow.ErrPending) {
				if flow.SlowDown != nil && flow.SlowDown(err) {
					a.grokLogins.update(id, func(login *deviceLogin) {
						if login != nil && login.status == "pending" {
							login.interval += 5 * time.Second
						}
					})
				}
				continue
			}
			slog.Warn("Grok device authorization failed", "login_id", id, "error", err)
			a.grokLogins.finish(id, "failed", "Grok authorization failed", 0)
			return
		}
		acc, err := flow.Build(ctx, cfg, id, current, grant)
		if err != nil {
			slog.Warn("Grok device authorization produced no account", "login_id", id, "error", err)
			a.grokLogins.finish(id, "failed", "Grok authorization failed", 0)
			return
		}
		existing, err := a.saveNewAccountUnlessDuplicate(ctx, acc)
		if err != nil {
			slog.Warn("Grok device authorization could not save account", "login_id", id, "error", err)
			a.grokLogins.finish(id, "failed", "Grok authorization succeeded but account could not be saved", 0)
			return
		}
		if existing != nil {
			// A fresh device grant may rotate the durable refresh token. The
			// channel updates the matching identity in place, retaining operator
			// settings and accumulated runtime state already held on the row.
			if flow.Merge != nil {
				flow.Merge(existing, acc)
			}
			updateCtx, updateCancel := context.WithTimeout(ctx, 20*time.Second)
			if err := a.store.UpdateAccount(updateCtx, existing); err != nil {
				updateCancel()
				slog.Warn("Grok device authorization could not update account", "login_id", id, "account_id", existing.ID, "error", err)
				a.grokLogins.finish(id, "failed", "Grok authorization succeeded but account could not be updated", 0)
				return
			}
			updateCancel()
			a.grokLogins.finish(id, "complete", "Grok account credentials refreshed", existing.ID)
			return
		}
		a.grokLogins.finish(id, "complete", "Grok account added", acc.ID)
		a.syncAccountAfterCreate(*acc)
		return
	}
}
