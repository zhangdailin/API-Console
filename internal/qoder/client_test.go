package qoder

import (
	"context"
	"errors"
	"fmt"

	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// signedTestAccount builds an account whose credential and derived pair are
// complete, so a request can be assembled without touching the token endpoints.
// observedSnapshot renders the snapshot an upstream refresh records, including
// the wire fields the chat request's model block is rebuilt from.
func observedSnapshot() []string {
	enabled := true
	return CatalogSnapshot(newCatalog([]modelEntry{
		{Key: "qmodel_latest", Name: "Qwen3.7-Max", Format: "openai", Source: "system", Enable: &enabled, MaxInputTokens: 1000000},
		{Key: "qmodel_38max", Name: "Qwen3.8-Max", Format: "openai", Source: "system", Enable: &enabled, IsReasoning: true, MaxInputTokens: 1000000},
		{Key: "dmodel", Name: "DeepSeek-V4-Pro", Format: "openai", Source: "system", Enable: &enabled, IsReasoning: true, MaxInputTokens: 1000000},
	}))
}

func signedTestAccount() *store.Account {
	return &store.Account{
		ID:                1,
		AccountType:       "qoder",
		QoderAccessToken:  "access-1",
		QoderRefreshToken: "refresh-1",
		QoderExpiresAt:    time.Now().Add(6 * time.Hour),
		QoderUserID:       "uid-1",
		QoderMachineID:    "11111111-2222-4333-8444-555555555555",
		QoderRuntimeInfo:  "runtime-info",
		QoderRuntimeKey:   "runtime-key",
		QoderDataPolicy:   true,
		// Routing resolves against the observed catalog, so a client that signs
		// a request needs the snapshot a refresh would have recorded.
		QoderModelIDs: observedSnapshot(),
	}
}

func TestConcurrentRuntimeDerivationIsSingleFlight(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	acc.ID = 0
	acc.QoderRuntimeInfo = ""
	acc.QoderRuntimeKey = ""
	client := NewFromAccount(acc, nil)
	setTestEntropy(client, strings.NewReader(strings.Repeat("runtime-entropy-", 128)))

	const callers = 24
	results := make(chan RuntimeFields, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fields, err := client.ensureRuntimeFields(context.Background(), client.currentCredentials())
			results <- fields
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		testutil.NoError(t, err)
	}
	want := <-results
	testutil.False(t, !want.Complete(), "derived runtime fields are incomplete")
	for got := range results {
		testutil.Equal(t, got, want)
	}
}

func TestConcurrentExpiredCredentialRefreshesOnlyOnce(t *testing.T) {
	t.Parallel()

	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "deviceToken/refresh") {
			http.NotFound(w, r)
			return
		}
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_token":"access-new","refresh_token":"refresh-new","expires_in":7200}`))
	}))
	defer server.Close()

	acc := signedTestAccount()
	acc.ID = 0
	acc.QoderAccessToken = "access-old"
	acc.QoderRefreshToken = "refresh-old"
	acc.QoderExpiresAt = time.Now().Add(-time.Minute)
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)

	const callers = 24
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			creds, err := client.ensureAccessToken(context.Background())
			if err == nil && creds.AccessToken != "access-new" {
				err = fmt.Errorf("access token = %q", creds.AccessToken)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		testutil.NoError(t, err)
	}
	testutil.Equal(t, refreshes.Load(), 1)
}

func TestForceRefreshSkipsCredentialAlreadyRotatedByPeer(t *testing.T) {
	t.Parallel()
	acc := signedTestAccount()
	acc.ID = 0
	acc.QoderAccessToken = "access-new"
	acc.QoderRefreshToken = "refresh-new"
	client := NewFromAccount(acc, nil)
	rejected := Credentials{AccessToken: "access-old", RefreshToken: "refresh-old"}
	testutil.NoError(t, client.forceRefresh(context.Background(), rejected), "peer-rotated credential should be reused: %v")
	testutil.Equal(t, client.currentCredentials().AccessToken, "access-new")
}

type failingQoderUpdater struct {
	fail  bool
	calls int
}

func (f *failingQoderUpdater) UpdateQoderAccount(context.Context, int64, store.QoderAccountPatch) error {
	f.calls++
	if f.fail {
		return errors.New("write failed")
	}
	return nil
}

func TestRefreshReportsPersistenceFailureAndRetriesWriteBeforeReuse(t *testing.T) {
	t.Parallel()
	refreshes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes++
		_, _ = w.Write([]byte(`{"device_token":"access-new","refresh_token":"refresh-new","expires_in":7200}`))
	}))
	defer server.Close()

	acc := signedTestAccount()
	acc.QoderAccessToken = "access-old"
	acc.QoderRefreshToken = "refresh-old"
	acc.QoderExpiresAt = time.Now().Add(-time.Minute)
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)
	updater := &failingQoderUpdater{fail: true}
	client.SetAccountStore(updater)
	_, err := client.ensureAccessToken(context.Background())
	testutil.Error(t, err)
	updater.fail = false
	creds, err := client.ensureAccessToken(context.Background())
	testutil.NoError(t, err)
	testutil.Equal(t, creds.AccessToken, "access-new")
	testutil.Equal(t, refreshes, 1)
	testutil.Equal(t, updater.calls, 2)
}

// TestSendRequestDoesNotReplayAfterOutput proves a retry is refused once content
// has reached the caller: replaying would duplicate the answer.
func TestSendRequestDoesNotReplayAfterOutput(t *testing.T) {
	t.Parallel()

	var chatCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "deviceToken/refresh"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"device_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`))
		case strings.Contains(r.URL.Path, "agent_chat_generation"):
			chatCalls++
			w.Header().Set("Content-Type", "text/event-stream")
			// Emit a delta and then fail without a finish event.
			_, _ = w.Write([]byte(envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"partial"}}]}`)))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "Qwen3.7-Max",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	testutil.False(t, err == nil, "SendRequestWithPayload() error = nil for a truncated stream")
	testutil.Falsef(t, !errors.Is(err, ErrStreamTruncated), "error = %v, want ErrStreamTruncated", err)
	testutil.Equal(t, chatCalls, 1)
	testutil.Equal(t, len(events), 1)
}

// TestEnsureRuntimeFieldsRequiresIdentity proves the derivation is refused
// before the UID is known, instead of encrypting an empty identity the gateway
// would reject.
func TestEnsureRuntimeFieldsRequiresIdentity(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	acc.QoderRuntimeInfo = ""
	acc.QoderRuntimeKey = ""
	acc.QoderUserID = ""
	client := NewFromAccount(acc, nil)
	_, err := client.ensureRuntimeFields(context.Background(), credsOf(acc))
	testutil.Error(t, err)
}

func credsOf(acc *store.Account) Credentials { return ResolveCredentials(acc) }

// TestEntitlementRefusalKeepsTheAccountUsable drives one real streaming request
// against a stub that answers exactly what a live Qoder account without a
// subscription answers, then feeds the resulting error to the shared account
// classifier the handler uses.
//
// This is the whole bug in one assertion. On the live deployment the classifier
// read the upstream "403" out of the message, marked the account status "403",
// and the console showed 「禁止访问」 — and the channel raised a "no usable
// account" alarm — for an account whose credential the gateway had just accepted.
func TestEntitlementRefusalKeepsTheAccountUsable(t *testing.T) {
	t.Parallel()

	// Verbatim from the live gateway: HTTP 200, a business status of 403, and a
	// body naming the pricing page.
	inner := `{"code":"112","message":"{\"pricingUrl\":\"https://qoder.com/pricing?client=qoder\"}"}`
	envelopeBody, err := json.Marshal(map[string]any{
		"headers":         map[string][]string{"Content-Type": {"application/json"}},
		"body":            inner,
		"statusCodeValue": 403,
		"statusCode":      "FORBIDDEN",
	})
	testutil.NoError(t, err, "marshal fixture: %v")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data:" + string(envelopeBody) + "\n\n"))
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)

	requestErr := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "Qwen3.7-Max",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, nil, nil)
	testutil.False(t, requestErr == nil, "SendRequestWithPayload() error = nil, want an entitlement refusal")
	testutil.Falsef(t, !errors.Is(requestErr, ErrNoEntitlement), "error = %v, want ErrNoEntitlement", requestErr)

	// The handler's own classifier, on the handler's own input.
	testutil.Equal(t, apperrors.ClassifyAccountStatus(requestErr.Error()), "")

	// And the reason still reaches whoever reads the request error.
	testutil.MustContain(t, requestErr.Error(), "pricing")
}

// A subscription refusal is terminal for this account/model pair, but another
// account may have a different plan. Do not retry on the same account or mark
// its credential bad; the handler switches once and records a model cooldown.
func TestEntitlementRefusalSwitchesAccounts(t *testing.T) {
	t.Parallel()

	err := entitlementError(`{"code":"112","message":"{\"pricingUrl\":\"https://qoder.com/pricing?client=qoder\"}"}`)
	class := apperrors.ClassifyUpstreamError(err.Error())
	testutil.Falsef(t, class.Category != "model_unavailable" || !class.Retryable || !class.SwitchAccount, "classification = %+v, want switchable model_unavailable", class)
	testutil.Equal(t, apperrors.ClassifyAccountStatus(err.Error()), "")
}
