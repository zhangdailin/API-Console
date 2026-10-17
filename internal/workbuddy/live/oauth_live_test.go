//go:build live

package live

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"orchids-api/internal/testutil"
	"orchids-api/internal/workbuddy"
)

// TestLive_StartAuthLogin exercises the real authorization bootstrap: it must
// return the official login page for a fresh state and report the transaction as
// pending until a browser completes it (which this test never does). The
// bootstrap endpoint is unauthenticated and creates only a short-lived state, so
// unlike the catalog/chat checks it needs no credential — only outbound network.
// It requires both -tags live and WB_LIVE=1, and still skips in short mode.
func TestLive_StartAuthLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("live check skipped in short mode")
	}
	if os.Getenv("WB_LIVE") != "1" {
		t.Skip("set WB_LIVE=1 to talk to the real Workbuddy authorization service")
	}
	client := workbuddy.NewFromAccount(nil, nil)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	state, authURL, err := client.StartAuthLogin(ctx, workbuddy.LoginClientVersion)
	testutil.NoError(t, err, "StartAuthLogin() error = %v")
	testutil.NotEqual(t, state, "")
	parsed, err := url.Parse(authURL)
	testutil.NoError(t, err, "login URL is not parseable: %v")
	testutil.Equal(t, parsed.Host, "www.workbuddy.ai")
	testutil.Equal(t, parsed.Path, "/login")
	testutil.Equal(t, parsed.Query().Get("state"), state)
	testutil.Equal(t, parsed.Query().Get("platform"), "workbuddy-ai")
	testutil.Equal(t, parsed.Query().Get("version"), workbuddy.LoginClientVersion)
	t.Logf("state=%s url=%s", state, authURL)

	_, err = client.PollAuthLogin(ctx, state)
	testutil.Error(t, err, "PollAuthLogin should return a pending error")
	testutil.Falsef(t, !errors.Is(err, workbuddy.ErrAuthPending), "PollAuthLogin() error = %v, want ErrAuthPending", err)
}
