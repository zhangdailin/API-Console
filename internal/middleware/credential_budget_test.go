package middleware

import (
	"context"
	"testing"
	"time"
)

func TestCredentialBackendFailureDoesNotLockOutValidLogins(t *testing.T) {
	limiter := &RateLimiter{maxAttempts: 1, window: time.Hour}
	for range 6 {
		valid, allowed, err := limiter.CheckCredentialsWithError("source", func() (bool, error) { return false, context.DeadlineExceeded })
		if valid || !allowed || err != context.DeadlineExceeded {
			t.Fatal("backend outage spent the password failure budget")
		}
	}
	valid, allowed, err := limiter.CheckCredentialsWithError("source", func() (bool, error) { return true, nil })
	if !valid || !allowed || err != nil {
		t.Fatal("healthy authentication remained locked out after recovery")
	}
	limiter.CheckCredentialsWithError("source", func() (bool, error) { return false, nil })
	_, allowed, err = limiter.CheckCredentialsWithError("source", func() (bool, error) { t.Fatal("exhausted budget verified credentials"); return true, nil })
	if allowed || err != nil {
		t.Fatal("incorrect passwords did not spend the failure budget")
	}
}
