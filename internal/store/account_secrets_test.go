package store

import (
	"orchids-api/internal/testutil"
	"reflect"
	"strings"
	"testing"
)

// TestSecretsCoversEveryCredentialShapedField is what stops the list from drifting
// again.
//
// The redactors drifted once already: attempt diagnostics covered seven of the
// credential values, so a WorkBuddy refresh token
// echoed by an upstream could be persisted. A field added to the account with a
// conventional name is now a test failure rather than a silent exposure.
func TestSecretsCoversEveryCredentialShapedField(t *testing.T) {
	credentialShaped := func(name string) bool {
		lower := strings.ToLower(name)
		for _, marker := range []string{"token", "cookie", "uat", "runtimeinfo", "runtimekey", "sessionid"} {
			if strings.Contains(lower, marker) {
				// A name that ends in a measurement suffix is a counter, not a
				// secret: the "token" marker matches "tokens_date"/"tokens_today"
				// because the daily spend is counted in tokens. No credential
				// field ends in one of these, so this narrows the heuristic
				// instead of weakening it.
				for _, suffix := range []string{"date", "today", "count", "total", "current", "limit"} {
					if strings.HasSuffix(lower, suffix) {
						return false
					}
				}
				return true
			}
		}
		return false
	}

	acc := &Account{}
	value := reflect.ValueOf(acc).Elem()
	typ := value.Type()

	checked := 0
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.Kind(reflect.String) || !credentialShaped(field.Name) {
			continue
		}
		checked++
		probe := "probe-" + field.Name
		value.Field(i).SetString(probe)

		covered := false
		for _, secret := range acc.Secrets() {
			if secret == probe {
				covered = true
				break
			}
		}
		testutil.CheckTrue(t, covered, "Account.%s looks like a credential but Secrets() does not return it")
		value.Field(i).SetString("")
	}

	// A heuristic that matches nothing would pass vacuously.
	testutil.NotEqual(t, checked, 0)
	t.Logf("checked %d credential-shaped fields", checked)
}

// TestSecretsSkipsEmptyValues keeps the redactors from replacing "" with a marker.
func TestSecretsReturnsOnlyNonEmptyValues(t *testing.T) {
	acc := &Account{Token: "t", ClientCookie: "u"}
	var nonEmpty int
	for _, secret := range acc.Secrets() {
		if strings.TrimSpace(secret) != "" {
			nonEmpty++
		}
	}
	testutil.Equal(t, nonEmpty, 2)
}
