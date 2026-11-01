package grok

import (
	"fmt"
	"testing"
)

func TestCLISubscriptionTierJWT(t *testing.T) {
	for n, want := range []string{"free", "supergrok", "x_basic", "x_premium", "x_premium_plus", "supergrok_heavy", "supergrok_lite"} {
		for _, raw := range []string{fmt.Sprint(n), fmt.Sprintf("%q", fmt.Sprint(n))} {
			if got := cliSubscriptionTierFromJWT(jwtWithClaims(t, `{"tier":`+raw+`}`)); got != want {
				t.Errorf("tier %s = %q, want %q", raw, got, want)
			}
		}
	}
	for _, raw := range []string{`-1`, `7`, `1.5`, `null`, `true`, `{}`, `"1.5"`, `"999999999999999999999999"`, `""`} {
		if got := cliSubscriptionTierFromJWT(jwtWithClaims(t, `{"tier":`+raw+`}`)); got != "" {
			t.Errorf("invalid tier %s = %q", raw, got)
		}
	}
	if got := cliSubscriptionTierFromJWT(jwtWithClaims(t, `{"tier":" SuperGrok Heavy "}`)); got != "SuperGrok Heavy" {
		t.Fatalf("named tier = %q", got)
	}
	for _, token := range []string{"", "opaque", "a.bad.b", "a.e30.b"} {
		if got := cliSubscriptionTierFromJWT(token); got != "" {
			t.Errorf("malformed or missing tier = %q", got)
		}
	}
}
