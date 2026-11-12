package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

const (
	maxRequestBody       = 64 << 20
	maxAccountAttempts   = 3
	maxBufferedResponse  = 64 << 20
	rateLimitStatusValue = "429"
)

// Handler serves the Console plane's inference, media and voice endpoints.
// Console is stateless: no response is stored, so previous_response_id and
// the Responses resource endpoints are not offered for Console routes.
type Handler struct {
	lb      *loadbalancer.LoadBalancer
	tracker loadbalancer.ConnTracker

	mu     sync.RWMutex
	cfg    *config.Config
	client *Client
}

// NewHandler builds the Console plane handler.
func NewHandler(cfg *config.Config, lb *loadbalancer.LoadBalancer) *Handler {
	return &Handler{lb: lb, tracker: loadbalancer.NewMemoryConnTracker(), cfg: cfg, client: NewClient(cfg)}
}

// SetConfig replaces the config and the client derived from it.
func (h *Handler) SetConfig(cfg *config.Config) {
	if h == nil || cfg == nil {
		return
	}
	client := NewClient(cfg)
	h.mu.Lock()
	h.cfg, h.client = cfg, client
	h.mu.Unlock()
}

func (h *Handler) consoleClient() *Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.client
}

// Client returns the current Console client.
func (h *Handler) Client() *Client { return h.consoleClient() }

func (h *Handler) store() *store.Store {
	if h == nil || h.lb == nil {
		return nil
	}
	return h.lb.Store
}

// route resolves a public Console model id against the published catalog.
func (h *Handler) route(ctx context.Context, modelID, capability string) (*store.Model, error) {
	st := h.store()
	if st == nil {
		return nil, errors.New("model not found")
	}
	id := strings.ToLower(strings.TrimSpace(modelID))
	row, err := st.GetModelByChannelAndModelID(ctx, "grok", id)
	if err != nil || row == nil || !row.Verified || !strings.EqualFold(row.Provider, ProviderConsole) {
		return nil, errors.New("model not found")
	}
	if !row.Status.Enabled() {
		return nil, errors.New("model not available")
	}
	if capability != "" && !row.SupportsCapability(capability) {
		return nil, fmt.Errorf("model %s does not support %s", id, capability)
	}
	return row, nil
}

// Route reports the upstream model of a Console route with the capability.
func (h *Handler) Route(ctx context.Context, modelID, capability string) (string, bool) {
	row, err := h.route(ctx, modelID, capability)
	if err != nil {
		return "", false
	}
	return row.UpstreamModel, true
}

// Session is one reserved Console account.
type Session struct {
	Account *store.Account
	Token   string
	release func()
}

// Close releases the account's connection slot.
func (s *Session) Close() {
	if s != nil && s.release != nil {
		s.release()
		s.release = nil
	}
}

// OpenSession selects a Console account whose catalog lists the model.
func (h *Handler) OpenSession(ctx context.Context, row *store.Model, exclude []int64) (*Session, error) {
	if h == nil || h.lb == nil {
		return nil, errors.New("load balancer not configured")
	}
	upstream := strings.TrimSpace(row.UpstreamModel)
	filter := func(acc *store.Account) error {
		if !IsConsoleAccount(acc) || !store.AccountAuthActive(acc) || SSOToken(acc) == "" || !row.AllowsAccount(acc.ID) {
			return loadbalancer.ErrAccountNotEligible
		}
		for _, listed := range acc.GrokConsoleModels {
			if strings.EqualFold(listed, upstream) {
				return nil
			}
		}
		return loadbalancer.ErrModelUnavailable
	}
	acc, err := h.lb.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, exclude, "grok", h.tracker, filter)
	if err != nil {
		return nil, err
	}
	if acc == nil {
		return nil, errors.New("no Grok Console account is available for " + row.ModelID)
	}
	h.tracker.Acquire(acc.ID)
	var once sync.Once
	return &Session{Account: acc, Token: SSOToken(acc), release: func() { once.Do(func() { h.tracker.Release(acc.ID) }) }}, nil
}

// switchable reports a failure another account may not repeat.
func switchable(err error) bool {
	if errors.Is(err, ErrUnauthorized) {
		return true
	}
	var status *StatusError
	if errors.As(err, &status) {
		return status.Status == http.StatusTooManyRequests || status.Status == http.StatusForbidden || status.Status == http.StatusPaymentRequired || status.Status >= 500
	}
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// noteFailure records what a failed attempt says about the account. Only a
// rate limit changes its state here; a refused session is re-verified by the
// scheduled maintenance, which owns the credential verdict.
func (h *Handler) noteFailure(ctx context.Context, acc *store.Account, err error) {
	var status *StatusError
	if errors.As(err, &status) && status.Status == http.StatusTooManyRequests && h.lb != nil {
		h.lb.MarkAccountStatus(ctx, acc, rateLimitStatusValue)
	}
}

// Call runs do on Console accounts until one answers, switching accounts on a
// failure another account may not repeat. The returned session must be
// closed after the response body.
func (h *Handler) Call(ctx context.Context, row *store.Model, do func(*Session) (*http.Response, error)) (*http.Response, *Session, error) {
	var exclude []int64
	var lastErr error
	for range maxAccountAttempts {
		sess, err := h.OpenSession(ctx, row, exclude)
		if err != nil {
			if lastErr != nil {
				return nil, nil, lastErr
			}
			return nil, nil, err
		}
		resp, err := do(sess)
		if err == nil {
			return resp, sess, nil
		}
		sess.Close()
		h.noteFailure(ctx, sess.Account, err)
		slog.Warn("grok console request failed", "account_id", sess.Account.ID, "model", row.ModelID, "error", redact(err, sess.Account))
		lastErr = err
		if !switchable(err) {
			break
		}
		exclude = append(exclude, sess.Account.ID)
	}
	return nil, nil, lastErr
}

// writeFailure answers a failed Console call without upstream credentials.
func writeFailure(w http.ResponseWriter, err error) {
	var status *StatusError
	switch {
	case errors.As(err, &status):
		if status.RetryAfter != "" && status.Status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", status.RetryAfter)
		}
		code := status.Status
		if code < 400 {
			code = http.StatusBadGateway
		}
		message := util.FirstNonEmpty(status.Message, fmt.Sprintf("Grok Console returned status %d", status.Status))
		httpserver.WriteErrorCode(w, code, httpserver.ErrorCodeForStatus(code), message)
	case errors.Is(err, ErrUnauthorized):
		httpserver.WriteErrorCode(w, http.StatusServiceUnavailable, "upstream_unauthorized", "No Grok Console account accepted the request; the accounts need a new SSO cookie")
	case errors.Is(err, loadbalancer.ErrModelUnavailable), strings.Contains(strings.ToLower(err.Error()), "no enabled accounts"), strings.Contains(err.Error(), "no Grok Console account"):
		httpserver.WriteErrorCode(w, http.StatusServiceUnavailable, "no_available_account", "No Grok Console account is available for this model")
	default:
		httpserver.WriteErrorCode(w, http.StatusBadGateway, "upstream_error", "The Grok Console request failed")
	}
}

// relay copies a 2xx upstream response to the client, flushing as it goes
// so a stream reaches the client event by event.
func relay(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for _, name := range []string{"Content-Type", "Content-Disposition", "Cache-Control"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// readModelRequest reads a JSON body and its model field.
func readModelRequest(w http.ResponseWriter, r *http.Request) (map[string]any, string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var payload map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "the request body must be a JSON object")
		return nil, "", false
	}
	model, _ := payload["model"].(string)
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, "", false
	}
	return payload, model, true
}

// resolveForRequest validates the API key's access and the route.
func (h *Handler) resolveForRequest(w http.ResponseWriter, r *http.Request, model, capability string) (*store.Model, bool) {
	if !httpserver.RequireAPIKeyModel(w, r, model) {
		return nil, false
	}
	row, err := h.route(r.Context(), model, capability)
	if err != nil {
		if err.Error() == "model not found" {
			httpserver.WriteErrorCode(w, http.StatusNotFound, "model_not_found", "The model `"+model+"` does not exist or you do not have access to it.")
		} else {
			httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		}
		return nil, false
	}
	return row, true
}

// forwardJSON sends a JSON payload with the upstream model name and relays
// the answer.
func (h *Handler) forwardJSON(w http.ResponseWriter, r *http.Request, row *store.Model, path string, payload map[string]any, accept string) {
	payload["model"] = row.UpstreamModel
	body, err := json.Marshal(payload)
	if err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid request")
		return
	}
	resp, sess, err := h.Call(r.Context(), row, func(sess *Session) (*http.Response, error) {
		return h.consoleClient().Do(r.Context(), sess.Token, http.MethodPost, path, body, "application/json", accept)
	})
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer sess.Close()
	relay(w, resp)
}

// HandleChatCompletions serves POST /chat/completions for a Console route.
func (h *Handler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	payload, model, ok := readModelRequest(w, r)
	if !ok {
		return
	}
	r = r.WithContext(middleware.WithRequestModel(r.Context(), model))
	row, ok := h.resolveForRequest(w, r, model, store.CapabilityChat)
	if !ok {
		return
	}
	h.forwardJSON(w, r, row, "/chat/completions", payload, "")
}

// HandleResponses serves POST /responses for a Console route. Console keeps
// no conversation state, so the request is sent with store=false and a
// previous_response_id is refused instead of being dropped silently.
func (h *Handler) HandleResponses(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	payload, model, ok := readModelRequest(w, r)
	if !ok {
		return
	}
	r = r.WithContext(middleware.WithRequestModel(r.Context(), model))
	if previous, _ := payload["previous_response_id"].(string); strings.TrimSpace(previous) != "" {
		httpserver.WriteErrorCode(w, http.StatusBadRequest, "invalid_request_error", "Grok Console routes are stateless: previous_response_id is not supported; send the full input")
		return
	}
	row, ok := h.resolveForRequest(w, r, model, store.CapabilityResponses)
	if !ok {
		return
	}
	payload["store"] = false
	h.forwardJSON(w, r, row, "/responses", payload, "")
}

// HandleImagesGenerations serves POST /images/generations (JSON).
func (h *Handler) HandleImagesGenerations(w http.ResponseWriter, r *http.Request) {
	h.handleImages(w, r, "/images/generations", store.CapabilityImage)
}

// HandleImagesEdits serves POST /images/edits with a JSON body (the xAI
// image shape: an image URL or data URI). Multipart uploads are not accepted
// for Console routes.
func (h *Handler) HandleImagesEdits(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/") {
		httpserver.WriteErrorCode(w, http.StatusUnsupportedMediaType, "invalid_request_error", "Grok Console image edits take a JSON body with the image as a URL or data URI")
		return
	}
	h.handleImages(w, r, "/images/edits", store.CapabilityImageEdit)
}

func (h *Handler) handleImages(w http.ResponseWriter, r *http.Request, path, capability string) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	payload, model, ok := readModelRequest(w, r)
	if !ok {
		return
	}
	row, ok := h.resolveForRequest(w, r, model, capability)
	if !ok {
		return
	}
	h.forwardJSON(w, r, row, path, payload, "application/json")
}

// DoJSON sends one JSON request on the session's account and returns the
// body of a 2xx answer.
func (h *Handler) DoJSON(ctx context.Context, sess *Session, method, path string, payload any) ([]byte, error) {
	var body []byte
	if payload != nil {
		var err error
		if body, err = json.Marshal(payload); err != nil {
			return nil, err
		}
	}
	resp, err := h.consoleClient().Do(ctx, sess.Token, method, path, body, "application/json", "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBufferedResponse+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBufferedResponse {
		return nil, errors.New("the Console response is too large")
	}
	return bytes.TrimSpace(raw), nil
}

// withTimeout bounds a background Console operation.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}
