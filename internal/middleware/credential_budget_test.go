package middleware

import (
	"context"
	"testing"
	"time"
)

func TestCredentialBackendFailureDoesNotLockOutValidLogins(t *testing.T) {
	limiter := &RateLimiter{maxAttempts: 1, window: time.Hour}
	for i := 0; i < 6; i++ {
		valid, allowed, err := limiter.CheckCredentialsWithError("source", func() (bool, error) { return false, context.DeadlineExceeded })
		if valid || !allowed || err != context.DeadlineExceeded {
			t.Fatal("backend outage spent the password failure budget")
		}
	}
	valid, allowed, err := limiter.CheckCredentialsWithError("source", func() (bool, error) { return true, nil })
	if !valid || !allowed || err != nil {
		t.Fatal("healthy authentication remained locked out after recovery")
	}
	limiter.CheckCredentials("source", func() bool { return false })
	_, allowed = limiter.CheckCredentials("source", func() bool { t.Fatal("exhausted budget verified credentials"); return true })
	if allowed {
		t.Fatal("incorrect passwords did not spend the failure budget")
	}
}
