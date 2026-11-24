package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"orchids-api/internal/testutil"
)

func newDoubaoIdentityTestStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := New(Options{RedisAddr: mini.Addr(), RedisPrefix: "doubao-identity-test:", CredentialEncryptionKey: bytes.Repeat([]byte{0x64}, 32)})
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, mini
}

func doubaoIdentityTestAccount() *Account {
	observed := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	return &Account{
		AccountType: "doubao", Name: "identity test", ClientCookie: "synthetic-cookie-original",
		DoubaoDeviceID: "1234567890123456789", DoubaoWebID: "2234567890123456789", DoubaoTeaUUID: "2234567890123456789",
		DoubaoVerifiedAt: observed, VerifiedAt: observed, Enabled: false,
		AuthStatus: AccountAuthStatusReauthRequired, StatusCode: "429", StatusMessage: "synthetic throttle",
		QuotaResetAt: observed.Add(time.Hour), LastAttempt: observed, LastUsedAt: observed,
		RateLimitFailures: 3, Weight: 4, MaxConcurrent: 2, Subscription: "synthetic",
		UsageCurrent: 12, UsageLimit: 30, UsageTotal: 70, TokensToday: 9, TokensDate: "2030-01-02", RequestCount: 11,
		UserID: "synthetic-user", Email: "synthetic@example.invalid", AgentMode: "synthetic-mode",
		ModelCooldowns:       map[string]time.Time{"synthetic-model": observed.Add(time.Minute)},
		ModelCooldownReasons: map[string]ModelCooldownReason{"synthetic-model": ModelCooldownReason("synthetic reason")},
	}
}

func doubaoIdentityTestPatch(acc *Account) DoubaoIdentityPatch {
	return DoubaoIdentityPatch{
		ExpectedCookie: acc.ClientCookie, ExpectedDeviceID: acc.DoubaoDeviceID,
		ExpectedWebID: acc.DoubaoWebID, ExpectedTeaUUID: acc.DoubaoTeaUUID,
		DeviceID: "3234567890123456789", WebID: "4234567890123456789", TeaUUID: "4234567890123456789",
	}
}

func doubaoIdentityStoredRow(t *testing.T, mini *miniredis.Miniredis, id int64) string {
	t.Helper()
	raw, err := mini.Get(fmt.Sprintf("doubao-identity-test:accounts:id:%d", id))
	testutil.NoError(t, err)
	return raw
}

func TestDoubaoIdentityRotationOnlyChangesIdentityAndUpdatedAt(t *testing.T) {
	s, mini := newDoubaoIdentityTestStore(t)
	acc := doubaoIdentityTestAccount()
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	before, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	patch := doubaoIdentityTestPatch(before)
	testutil.NoError(t, s.RotateDoubaoIdentity(t.Context(), acc.ID, patch))
	after, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, after.UpdatedAt.After(before.UpdatedAt), "rotation did not advance the version")
	expected := before.Clone()
	expected.DoubaoDeviceID, expected.DoubaoWebID, expected.DoubaoTeaUUID = patch.DeviceID, patch.WebID, patch.TeaUUID
	expected.UpdatedAt = after.UpdatedAt
	testutil.True(t, reflect.DeepEqual(after, expected), "rotation changed unrelated account fields")
	// The existing encrypted account serializer must own the new values too.
	raw := doubaoIdentityStoredRow(t, mini, acc.ID)
	var row map[string]any
	testutil.NoError(t, json.Unmarshal([]byte(raw), &row))
	for _, field := range []string{"client_cookie", "doubao_device_id", "doubao_web_id", "doubao_tea_uuid"} {
		value, ok := row[field].(string)
		testutil.True(t, ok && strings.HasPrefix(value, encryptedCredentialPrefix), "identity or cookie was not encrypted")
	}
	for _, secret := range []string{before.ClientCookie, before.DoubaoDeviceID, before.DoubaoWebID, patch.DeviceID, patch.WebID} {
		testutil.True(t, !strings.Contains(raw, secret), "stored row exposed a plaintext identity or cookie")
	}
}

func TestDoubaoIdentityRotationRejectsEachStaleExpectation(t *testing.T) {
	for _, field := range []string{"cookie", "device", "web", "tea"} {
		t.Run(field, func(t *testing.T) {
			s, mini := newDoubaoIdentityTestStore(t)
			acc := doubaoIdentityTestAccount()
			testutil.NoError(t, s.CreateAccount(t.Context(), acc))
			patch := doubaoIdentityTestPatch(acc)
			switch field {
			case "cookie":
				patch.ExpectedCookie += " "
			case "device":
				patch.ExpectedDeviceID += " "
			case "web":
				patch.ExpectedWebID += " "
			case "tea":
				patch.ExpectedTeaUUID += " "
			}
			before := doubaoIdentityStoredRow(t, mini, acc.ID)
			err := s.RotateDoubaoIdentity(t.Context(), acc.ID, patch)
			testutil.Error(t, err)
			testutil.MustContain(t, err.Error(), "changed concurrently")
			testutil.True(t, before == doubaoIdentityStoredRow(t, mini, acc.ID), "failed comparison changed the stored row")
		})
	}
}

func TestDoubaoIdentityConcurrentRotationsHaveOneWinner(t *testing.T) {
	s, _ := newDoubaoIdentityTestStore(t)
	acc := doubaoIdentityTestAccount()
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	patches := []DoubaoIdentityPatch{doubaoIdentityTestPatch(acc), doubaoIdentityTestPatch(acc)}
	patches[1].DeviceID, patches[1].WebID, patches[1].TeaUUID = "5234567890123456789", "6234567890123456789", "6234567890123456789"
	start := make(chan struct{})
	errs := make(chan error, len(patches))
	var wg sync.WaitGroup
	for _, patch := range patches {
		wg.Go(func() {
			<-start
			errs <- s.RotateDoubaoIdentity(t.Context(), acc.ID, patch)
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		} else {
			testutil.MustContain(t, err.Error(), "changed concurrently")
		}
	}
	testutil.Equal(t, successes, 1)
	got, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	matches := 0
	for _, patch := range patches {
		if got.DoubaoDeviceID == patch.DeviceID && got.DoubaoWebID == patch.WebID && got.DoubaoTeaUUID == patch.TeaUUID {
			matches++
		}
	}
	testutil.Equal(t, matches, 1)
}

func TestDoubaoStaleSnapshotsPreserveRotatedIdentityAndCurrentCookie(t *testing.T) {
	for _, omitType := range []bool{false, true} {
		t.Run(fmt.Sprintf("omit-type-%t", omitType), func(t *testing.T) {
			s, _ := newDoubaoIdentityTestStore(t)
			acc := doubaoIdentityTestAccount()
			testutil.NoError(t, s.CreateAccount(t.Context(), acc))
			stale, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			patch := doubaoIdentityTestPatch(stale)
			testutil.NoError(t, s.RotateDoubaoIdentity(t.Context(), acc.ID, patch))
			// A fresh ordinary update may refresh the cookie without a new identity.
			current, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			current.ClientCookie = "synthetic-cookie-refreshed"
			testutil.NoError(t, s.UpdateAccount(t.Context(), current))
			stale.Name = "stale writer's ordinary edit"
			if omitType {
				stale.AccountType = ""
			}
			testutil.NoError(t, s.UpdateAccount(t.Context(), stale))
			got, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			testutil.Equal(t, got.Name, stale.Name)
			testutil.True(t, got.ClientCookie == current.ClientCookie, "stale snapshot restored an old cookie")
			testutil.True(t, got.DoubaoDeviceID == patch.DeviceID && got.DoubaoWebID == patch.WebID && got.DoubaoTeaUUID == patch.TeaUUID, "stale snapshot restored old browser identifiers")
		})
	}
}

func TestDoubaoFreshAndZeroTimestampLoginUpdatesReplaceIdentity(t *testing.T) {
	for _, zeroTimestamp := range []bool{false, true} {
		t.Run(fmt.Sprintf("zero-timestamp-%t", zeroTimestamp), func(t *testing.T) {
			s, _ := newDoubaoIdentityTestStore(t)
			acc := doubaoIdentityTestAccount()
			testutil.NoError(t, s.CreateAccount(t.Context(), acc))
			fresh, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			patch := doubaoIdentityTestPatch(fresh)
			fresh.ClientCookie = "synthetic-qr-relogin-cookie"
			fresh.DoubaoDeviceID, fresh.DoubaoWebID, fresh.DoubaoTeaUUID = patch.DeviceID, patch.WebID, patch.TeaUUID
			if zeroTimestamp {
				fresh.UpdatedAt = time.Time{}
			}
			testutil.NoError(t, s.UpdateAccount(t.Context(), fresh))
			got, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			testutil.True(t, got.ClientCookie == fresh.ClientCookie && got.DoubaoDeviceID == fresh.DoubaoDeviceID && got.DoubaoWebID == fresh.DoubaoWebID && got.DoubaoTeaUUID == fresh.DoubaoTeaUUID, "fresh login replacement was blocked")
		})
	}
}

func TestDoubaoIdentityRotationRejectsInvalidIdentities(t *testing.T) {
	invalid := []string{"", "0", "0000000000000000000", "0234567890123456789", "123456789012345678", "12345678901234567890", "123456789012345678x", "+234567890123456789", "123456789012345678 ", "１２３４５６７８９０１２３４５６７８９"}
	for _, field := range []string{"device", "web", "tea"} {
		for i, value := range invalid {
			t.Run(fmt.Sprintf("%s-invalid-%d", field, i), func(t *testing.T) {
				s, mini := newDoubaoIdentityTestStore(t)
				acc := doubaoIdentityTestAccount()
				testutil.NoError(t, s.CreateAccount(t.Context(), acc))
				patch := doubaoIdentityTestPatch(acc)
				switch field {
				case "device":
					patch.DeviceID = value
				case "web":
					patch.WebID, patch.TeaUUID = value, value
				case "tea":
					patch.TeaUUID = value
				}
				before := doubaoIdentityStoredRow(t, mini, acc.ID)
				testutil.Error(t, s.RotateDoubaoIdentity(t.Context(), acc.ID, patch))
				testutil.True(t, before == doubaoIdentityStoredRow(t, mini, acc.ID), "invalid identity changed the stored row")
			})
		}
	}
	for _, invalidCase := range []string{"unchanged-device", "unchanged-web", "mismatched-tea"} {
		t.Run(invalidCase, func(t *testing.T) {
			s, mini := newDoubaoIdentityTestStore(t)
			acc := doubaoIdentityTestAccount()
			testutil.NoError(t, s.CreateAccount(t.Context(), acc))
			patch := doubaoIdentityTestPatch(acc)
			switch invalidCase {
			case "unchanged-device":
				patch.DeviceID = acc.DoubaoDeviceID
			case "unchanged-web":
				patch.WebID, patch.TeaUUID = acc.DoubaoWebID, acc.DoubaoWebID
			case "mismatched-tea":
				patch.TeaUUID = patch.DeviceID
			}
			before := doubaoIdentityStoredRow(t, mini, acc.ID)
			testutil.Error(t, s.RotateDoubaoIdentity(t.Context(), acc.ID, patch))
			testutil.True(t, before == doubaoIdentityStoredRow(t, mini, acc.ID), "invalid rotation changed the stored row")
		})
	}
}

func TestDoubaoIdentityRotationRejectsOtherAccountTypes(t *testing.T) {
	for _, accountType := range []string{"", "warp", "grok", "doubao-other"} {
		t.Run("type-"+accountType, func(t *testing.T) {
			s, mini := newDoubaoIdentityTestStore(t)
			acc := doubaoIdentityTestAccount()
			acc.AccountType = accountType
			testutil.NoError(t, s.CreateAccount(t.Context(), acc))
			before := doubaoIdentityStoredRow(t, mini, acc.ID)
			testutil.Error(t, s.RotateDoubaoIdentity(t.Context(), acc.ID, doubaoIdentityTestPatch(acc)))
			testutil.True(t, before == doubaoIdentityStoredRow(t, mini, acc.ID), "rotation changed a non-Doubao row")
		})
	}
}

func TestDoubaoIdentityRotationRejectsMalformedStoredFieldTypes(t *testing.T) {
	for _, field := range []string{"account_type", "client_cookie", "doubao_device_id", "doubao_web_id", "doubao_tea_uuid"} {
		for _, value := range []string{"123", "true", "[]", "{}"} {
			t.Run(field+"-"+value, func(t *testing.T) {
				s, mini := newDoubaoIdentityTestStore(t)
				acc := doubaoIdentityTestAccount()
				testutil.NoError(t, s.CreateAccount(t.Context(), acc))
				var row map[string]json.RawMessage
				testutil.NoError(t, json.Unmarshal([]byte(doubaoIdentityStoredRow(t, mini, acc.ID)), &row))
				row[field] = json.RawMessage(value)
				body, err := json.Marshal(row)
				testutil.NoError(t, err)
				mini.Set(fmt.Sprintf("doubao-identity-test:accounts:id:%d", acc.ID), string(body))
				testutil.Error(t, s.RotateDoubaoIdentity(t.Context(), acc.ID, doubaoIdentityTestPatch(acc)))
				testutil.True(t, string(body) == doubaoIdentityStoredRow(t, mini, acc.ID), "malformed stored row was rewritten")
			})
		}
	}
}

func TestDoubaoIdentityRotationRequiresConfiguredBackend(t *testing.T) {
	var absent *Store
	for _, s := range []*Store{absent, {}} {
		testutil.Error(t, s.RotateDoubaoIdentity(t.Context(), 1, DoubaoIdentityPatch{}))
	}
}

func TestDoubaoIdentityMissingRowFailsClosed(t *testing.T) {
	s, _ := newDoubaoIdentityTestStore(t)
	patch := doubaoIdentityTestPatch(doubaoIdentityTestAccount())
	for _, id := range []int64{0, 991922} {
		if err := s.RotateDoubaoIdentity(t.Context(), id, patch); err != ErrNoRows {
			t.Fatalf("missing rotation id %d error=%v", id, err)
		}
	}
	acc := doubaoIdentityTestAccount()
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	patch = doubaoIdentityTestPatch(acc)
	if err := s.DeleteAccount(t.Context(), acc.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RotateDoubaoIdentity(t.Context(), acc.ID, patch); err != ErrNoRows {
		t.Fatalf("deleted row returned success: %v", err)
	}
}
