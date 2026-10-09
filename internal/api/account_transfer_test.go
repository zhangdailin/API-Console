package api

import (
	"bytes"
	"encoding/json"
	"github.com/alicebob/miniredis/v2"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"strings"
	"testing"
	"time"
)

func transferAPI(t *testing.T, key byte) (*API, *store.Store) {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "transfer:", CredentialEncryptionKey: bytes.Repeat([]byte{key}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s, "admin", "password", &config.Config{}), s
}
func importBackup(t *testing.T, a *API, body []byte) ImportResult {
	t.Helper()
	w := httptest.NewRecorder()
	a.HandleImport(w, httptest.NewRequest(http.MethodPost, "/api/import", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("import status %d: %s", w.Code, w.Body.String())
	}
	var result ImportResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestAccountBackupRejectsGenericCredentialSlots(t *testing.T) {
	a, s := transferAPI(t, 3)
	backup := ExportData{Version: 1, Accounts: []store.Account{
		{AccountType: "workbuddy", Token: "access", RefreshToken: "refresh"},
		{AccountType: "qoder", ClientCookie: `{"accessToken":"access","refreshToken":"refresh","machineId":"device"}`},
	}}
	body, err := json.Marshal(backup)
	if err != nil {
		t.Fatal(err)
	}
	result := importBackup(t, a, body)
	if result.Imported != 0 || result.Skipped != 2 {
		t.Fatalf("result: %+v", result)
	}
	rows, err := s.ListAccounts(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
}
func TestAccountBackupRestoresFourChannelsAcrossEncryptionKeys(t *testing.T) {
	source, s := transferAPI(t, 1)
	target, destination := transferAPI(t, 2)
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	rows := []store.Account{
		{AccountType: "workbuddy", WorkBuddyAccessToken: "wb-access", WorkBuddyRefreshToken: "wb-refresh", WorkBuddyUID: "wb-uid", WorkBuddyExpiresAt: expires},
		{AccountType: "qoder", QoderAccessToken: "q-access", QoderRefreshToken: "q-refresh", QoderUserID: "q-user", QoderMachineID: "q-device", QoderRuntimeInfo: "q-runtime", QoderRuntimeKey: "q-key", QoderExpiresAt: expires, QoderModelIDs: []string{"model-a"}},
		{AccountType: "cline", ClineAccessToken: "c-access", ClineRefreshToken: "c-refresh", ClineEmail: "test@example.com", ClineExpiresAt: expires},
		{AccountType: "grok", CredentialType: "oauth", GrokProvider: "build", OAuthAccessToken: "g-access", OAuthRefreshToken: "g-refresh", UserID: "g-user", OAuthExpiresAt: expires},
	}
	for i := range rows {
		rows[i].Name = rows[i].AccountType
		rows[i].Weight = 3
		rows[i].Enabled = true
		rows[i].RequestCount = 99
		rows[i].TokensToday = 123
		if err := s.CreateAccount(t.Context(), &rows[i]); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	source.HandleExport(w, httptest.NewRequest(http.MethodGet, "/api/export", nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("export failed: %d", w.Code)
	}
	var backup ExportData
	if err := json.Unmarshal(w.Body.Bytes(), &backup); err != nil {
		t.Fatal(err)
	}
	for _, row := range backup.Accounts {
		if row.ID != 0 || row.RequestCount != 0 {
			t.Fatal("instance IDs or counters exported")
		}
	}
	result := importBackup(t, target, w.Body.Bytes())
	if result.Imported != 4 || result.Skipped != 0 {
		t.Fatalf("result: %+v", result)
	}
	restored, err := destination.ListAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	byChannel := map[string]*store.Account{}
	for _, row := range restored {
		byChannel[row.AccountType] = row
		if row.RequestCount != 0 || row.TokensToday != 0 || row.Weight != 3 || !row.Enabled {
			t.Fatalf("state lost: %s", row.AccountType)
		}
	}
	for _, original := range rows {
		got := byChannel[original.AccountType]
		if got == nil || normalizedAccountCredentialKey(got) != normalizedAccountCredentialKey(&original) || stableProviderIdentityKey(got) != stableProviderIdentityKey(&original) {
			t.Fatalf("credential or identity lost: %s", original.AccountType)
		}
	}
	q := byChannel["qoder"]
	if q.QoderMachineID != "q-device" || q.QoderRuntimeInfo != "q-runtime" || q.QoderRuntimeKey != "q-key" || !q.QoderExpiresAt.Equal(expires) || len(q.QoderModelIDs) != 1 {
		t.Fatal("Qoder recovery material lost")
	}
	again := importBackup(t, target, w.Body.Bytes())
	if again.Imported != 0 || again.Duplicates != 4 {
		t.Fatalf("repeat import duplicated rows: %+v", again)
	}
	backup.Accounts[0].Name = "do not overwrite"
	for i := range backup.Accounts {
		if backup.Accounts[i].AccountType == "workbuddy" {
			backup.Accounts[i].WorkBuddyRefreshToken = "rotated-other"
		}
	}
	encoded, _ := json.Marshal(backup)
	again = importBackup(t, target, encoded)
	if again.Duplicates != 4 {
		t.Fatal("stable identity not detected")
	}
	restored, _ = destination.ListAccounts(t.Context())
	if len(restored) != 4 {
		t.Fatal("duplicate rows inserted")
	}
}
func TestAccountImportRejectsInvalidFilesAndReportsInvalidRows(t *testing.T) {
	a, _ := transferAPI(t, 1)
	for _, body := range []string{`{}`, `{"version":2,"accounts":[]}`, `{"version":1,"accounts":[]} {}`, `{"version":1,"accounts":null}`} {
		w := httptest.NewRecorder()
		a.HandleImport(w, httptest.NewRequest(http.MethodPost, "/api/import", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("invalid file accepted: %d", w.Code)
		}
	}
	body := []byte(`{"version":1,"accounts":[{"account_type":"workbuddy"},{"account_type":"qoder","qoder_refresh_token":"refresh"},{"account_type":"cline"},{"account_type":"grok","token":"retired-cookie"},{"account_type":"warp"}]}`)
	result := importBackup(t, a, body)
	if result.Invalid != 5 || result.Imported != 0 || len(result.Issues) != 5 {
		t.Fatalf("invalid credentials accepted: %+v", result)
	}
	oversized := `{"version":1,"accounts":[],"padding":"` + strings.Repeat("x", 8<<20) + `"}`
	w := httptest.NewRecorder()
	a.HandleImport(w, httptest.NewRequest(http.MethodPost, "/api/import", strings.NewReader(oversized)))
	if w.Code != 413 {
		t.Fatalf("oversized file: %d", w.Code)
	}
}
func TestAccountImportDuplicatesWithinFileAreSkipped(t *testing.T) {
	a, _ := transferAPI(t, 1)
	body := []byte(`{"version":1,"accounts":[{"account_type":"cline","cline_refresh_token":"same"},{"account_type":"cline","cline_refresh_token":"same"}]}`)
	result := importBackup(t, a, body)
	if result.Imported != 1 || result.Duplicates != 1 {
		t.Fatalf("within-file duplicate: %+v", result)
	}
}
