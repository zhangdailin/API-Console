package auth

import (
	"context"
	"errors"
	"orchids-api/internal/testutil"
	"testing"
	"time"
)

type fakeSessionBackend struct {
	sessions  map[string]time.Time
	hasErr    error
	saveErr   error
	deleted   []string
	saveCalls int
}

func (f *fakeSessionBackend) SaveSession(_ context.Context, token string, expiry time.Time) error {
	f.saveCalls++
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.sessions == nil {
		f.sessions = map[string]time.Time{}
	}
	f.sessions[token] = expiry
	return nil
}

func (f *fakeSessionBackend) HasSession(_ context.Context, token string) (bool, error) {
	if f.hasErr != nil {
		return false, f.hasErr
	}
	_, ok := f.sessions[token]
	return ok, nil
}

func (f *fakeSessionBackend) DeleteSession(_ context.Context, token string) {
	delete(f.sessions, token)
	f.deleted = append(f.deleted, token)
}

// resetSessionState isolates the package-level session state between tests and
// restores whatever the process had before.
func resetSessionState(t *testing.T) {
	t.Helper()
	previousBackend := durableSessionBackend()
	clearInProcessSessions()
	SetSessionBackend(nil)
	t.Cleanup(func() { SetSessionBackend(previousBackend); clearInProcessSessions() })
}

func clearInProcessSessions() {
	globalSessionStore.mu.Lock()
	globalSessionStore.sessions = make(map[string]time.Time)
	globalSessionStore.mu.Unlock()
}

func TestGenerateSessionTokenWithoutBackend(t *testing.T) {
	resetSessionState(t)

	token, err := GenerateSessionToken()
	testutil.NoError(t, err, "GenerateSessionToken() error = %v")
	testutil.Equal(t, len(token), sessionTokenLength*2)
	testutil.False(t, !ValidateSessionToken(token), "a token generated in-process must validate")
	testutil.False(t, ValidateSessionToken("not-a-session"), "an unknown token must not validate")
	testutil.False(t, ValidateSessionToken(""), "an empty token must not validate")
}

// A deploy restarts the process. With a durable backend the operator's cookie
// must keep working instead of bouncing the admin UI back to the login page.
func TestDurableSessionSurvivesProcessRestart(t *testing.T) {
	resetSessionState(t)
	backend := &fakeSessionBackend{}
	SetSessionBackend(backend)

	token, err := GenerateSessionToken()
	testutil.NoError(t, err, "GenerateSessionToken() error = %v")
	testutil.Equal(t, backend.saveCalls, 1)

	// Simulate the restart: the new process has an empty in-process map.
	clearInProcessSessions()

	testutil.False(t, !ValidateSessionToken(token), "a durable session must still validate after the in-process map is dropped")
}

// A backend outage must not bypass distributed session revocation.
func TestValidateFailsClosedWhenBackendFails(t *testing.T) {
	resetSessionState(t)

	token, err := GenerateSessionToken()
	testutil.NoError(t, err, "GenerateSessionToken() error = %v")

	// The process holds a local session; adding an unavailable backend must
	// not let that local mirror bypass the durable revocation check.
	SetSessionBackend(&fakeSessionBackend{hasErr: errors.New("redis unavailable")})

	testutil.False(t, ValidateSessionToken(token), "a backend outage must not bypass revocation")
}

// A session this process never issued stays unvalidatable while the backend is
// unreachable: an outage must not become an open door for arbitrary cookies.
func TestValidateStillFailsClosedForUnknownTokensDuringAnOutage(t *testing.T) {
	resetSessionState(t)
	SetSessionBackend(&fakeSessionBackend{hasErr: errors.New("redis unavailable")})

	testutil.False(t, ValidateSessionToken("token-from-a-previous-process"), "an unknown token must not validate while the backend is unreachable")
}

// Even previously confirmed sessions require a fresh revocation check.
func TestBackendConfirmedSessionRequiresRevocationCheck(t *testing.T) {
	resetSessionState(t)
	backend := &fakeSessionBackend{}
	SetSessionBackend(backend)

	token, err := GenerateSessionToken()
	testutil.NoError(t, err, "GenerateSessionToken() error = %v")

	// Restart: the in-process mirror is gone and only the backend knows the
	// session. A successful read does not authorize later offline validation.
	clearInProcessSessions()
	testutil.False(t, !ValidateSessionToken(token), "the durable session must validate after a restart")

	backend.hasErr = errors.New("redis unavailable")
	testutil.False(t, ValidateSessionToken(token), "a cached session cannot bypass revocation during an outage")

	// Revocation still wins: an authoritative negative clears the record.
	backend.hasErr = nil
	delete(backend.sessions, token)
	testutil.False(t, ValidateSessionToken(token), "a revoked session must be rejected by a healthy backend")
}

func TestValidateRejectsSessionMissingFromBackend(t *testing.T) {
	resetSessionState(t)

	token, err := GenerateSessionToken()
	testutil.NoError(t, err, "GenerateSessionToken() error = %v")

	// A session the backend never saw (for example one that was revoked while
	// the process was down) must be rejected even when the in-process mirror
	// still remembers it.
	SetSessionBackend(&fakeSessionBackend{})

	testutil.False(t, ValidateSessionToken(token), "a session unknown to the durable backend must be rejected")
}

func TestInvalidateSessionTokenClearsBothStores(t *testing.T) {
	resetSessionState(t)
	backend := &fakeSessionBackend{}
	SetSessionBackend(backend)

	token, err := GenerateSessionToken()
	testutil.NoError(t, err, "GenerateSessionToken() error = %v")

	InvalidateSessionToken(token)

	testutil.False(t, memoryHasSession(token), "InvalidateSessionToken must drop the in-process session")
	_, ok := backend.sessions[token]
	testutil.False(t, ok, "InvalidateSessionToken must drop the durable session")
	testutil.Equal(t, len(backend.deleted), 1)
	testutil.Equal(t, backend.deleted[0], token)
}

func TestExpiredInProcessSessionIsRejected(t *testing.T) {
	resetSessionState(t)

	rememberSession("expired-token", time.Now().Add(-time.Minute))
	testutil.False(t, ValidateSessionToken("expired-token"), "an expired session must not validate")
	testutil.False(t, memoryHasSession("expired-token"), "an expired session should be evicted from the in-process map")
}

func TestGenerateSessionTokenRequiresDurableWrite(t *testing.T) {
	resetSessionState(t)
	SetSessionBackend(&fakeSessionBackend{saveErr: errors.New("redis read-only")})
	token, err := GenerateSessionToken()
	testutil.Error(t, err)
	testutil.Equal(t, token, "")
}

func TestCleanupExpiredSessionsKeepsLiveSession(t *testing.T) {
	resetSessionState(t)

	rememberSession("live-token", time.Now().Add(time.Hour))
	rememberSession("expired-token", time.Now().Add(-time.Second))

	cleanupExpiredSessions()

	testutil.False(t, !memoryHasSession("live-token"), "cleanup must keep a live session")
	testutil.False(t, memoryHasSession("expired-token"), "cleanup must drop an expired session")
}
