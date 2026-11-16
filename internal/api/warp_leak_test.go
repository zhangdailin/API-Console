package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func warpPlantedAccount(channel string) *store.Account {
	return &store.Account{
		Name: "metadata remains", AccountType: channel, Enabled: true,
		WarpAccessToken: marker + "warp-access", WarpRefreshToken: marker + "warp-refresh",
		WarpExpiresAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
		WarpModelIDs:  []string{"observed-model"}, WarpModelsSyncedAt: time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC),
		StatusMessage: "upstream echoed " + marker + "warp-access " + marker + "warp-refresh",
	}
}

func TestWarpAccountOutputDoubleStripsCredentials(t *testing.T) {
	for _, channel := range []string{"warp", "cline", "workbuddy", "grok", "doubao", "unknown"} {
		t.Run(channel, func(t *testing.T) {
			acc := warpPlantedAccount(channel)
			projection := normalizeAccountOutput(acc)
			testutil.Equal(t, projection.WarpAccessToken, "")
			testutil.Equal(t, projection.WarpRefreshToken, "")
			testutil.MustNotContain(t, projection.StatusMessage, marker)
			// Neither layer can mutate the live record.
			testutil.Equal(t, acc.WarpAccessToken, marker+"warp-access")
			testutil.Equal(t, acc.WarpRefreshToken, marker+"warp-refresh")
			// The final marshal must protect even callers that skipped projection,
			// including quota maps that plant credential keys back in the response.
			out := accountOutput{Account: acc, Quota: map[string]any{
				"warp_access_token": marker + "quota-access", "warp_refresh_token": marker + "quota-refresh",
			}}
			raw, err := json.Marshal(out)
			testutil.NoError(t, err)
			testutil.MustNotContain(t, string(raw), marker)
			var row map[string]any
			testutil.NoError(t, json.Unmarshal(raw, &row))
			for _, key := range []string{"warp_access_token", "warp_refresh_token"} {
				_, exists := row[key]
				testutil.False(t, exists, "Warp credential key survived marshal")
			}
		})
	}
}

func TestWarpExportKeepsMetadataButNeverPortableCredentials(t *testing.T) {
	for _, channel := range []string{"warp", "cline", "workbuddy", "grok", "doubao"} {
		t.Run(channel, func(t *testing.T) {
			a, s := transferAPI(t, 9)
			acc := warpPlantedAccount(channel)
			testutil.NoError(t, s.CreateAccount(t.Context(), acc))
			w := httptest.NewRecorder()
			a.HandleExport(w, httptest.NewRequest(http.MethodGet, "/api/export", nil))
			testutil.Equal(t, w.Code, http.StatusOK)
			testutil.Equal(t, w.Header().Get("Cache-Control"), "no-store")
			testutil.MustNotContain(t, w.Body.String(), marker)
			var payload ExportData
			testutil.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
			testutil.Equal(t, len(payload.Accounts), 1)
			exported := payload.Accounts[0]
			testutil.Equal(t, exported.Name, acc.Name)
			testutil.Equal(t, exported.WarpAccessToken, "")
			testutil.Equal(t, exported.WarpRefreshToken, "")
			testutil.True(t, exported.WarpExpiresAt.IsZero(), "non-portable expiry exported")
			if channel == "warp" {
				testutil.True(t, slices.Equal(exported.WarpModelIDs, acc.WarpModelIDs), "metadata-only export lost catalog")
			}
			stored, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			testutil.Equal(t, stored.WarpRefreshToken, acc.WarpRefreshToken)
		})
	}
}

func TestWarpNormalizationAlwaysClearsNonportableSlots(t *testing.T) {
	for _, channel := range []string{"warp", "cline", "workbuddy", "grok", "doubao", "unknown"} {
		t.Run(channel, func(t *testing.T) {
			acc := warpPlantedAccount(" " + strings.ToUpper(channel) + " ")
			acc.ReplaceWarpCredentials = true
			normalizePortableAccount(acc)
			testutil.Equal(t, acc.WarpAccessToken, "")
			testutil.Equal(t, acc.WarpRefreshToken, "")
			testutil.True(t, acc.WarpExpiresAt.IsZero(), "expiry survived portable normalization")
			testutil.False(t, acc.ReplaceWarpCredentials, "write intent survived normalization")
			testutil.MustNotContain(t, acc.StatusMessage, marker)
		})
	}
	// Foreign redaction remains explicit, independently of portability validation.
	foreign := warpPlantedAccount("cline")
	redactForeignCredentials(foreign)
	testutil.Equal(t, foreign.WarpAccessToken, "")
	testutil.Equal(t, foreign.WarpRefreshToken, "")
	testutil.True(t, foreign.WarpExpiresAt.IsZero(), "foreign expiry survived redaction")
}

func TestWarpArchiveImportCannotInstallCredentials(t *testing.T) {
	a, s := transferAPI(t, 7)
	body, err := json.Marshal(ExportData{Version: 1, Accounts: []store.Account{*warpPlantedAccount("warp")}})
	testutil.NoError(t, err)
	result := importBackup(t, a, body)
	testutil.Equal(t, result.Imported, 0)
	testutil.Equal(t, result.Invalid, 1)
	rows, err := s.ListAccounts(t.Context())
	testutil.NoError(t, err)
	testutil.Equal(t, len(rows), 0)
}
