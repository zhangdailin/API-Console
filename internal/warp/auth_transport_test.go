package warp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
)

func authResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

// Retain the production URL and redirect policy, but intercept every request.
func officialAuthClient(t *testing.T, handler func(*http.Request) (*http.Response, error)) *Client {
	t.Helper()
	c := NewFromAccount(testAccount(), &config.Config{WarpFirebaseAPIKey: "synthetic-project-config"})
	c.http.Transport = diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || (r.URL.Host != "securetoken.googleapis.com" && r.URL.Host != "identitytoolkit.googleapis.com" && r.URL.Host != "app.warp.dev") {
			t.Errorf("unexpected authentication destination")
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Error("authentication exchange is missing its short timeout")
		}
		if r.Header.Get("Authorization") != "" || r.URL.Query().Get("key") != "synthetic-project-config" {
			t.Error("incorrect authentication headers or public project configuration")
		}
		return handler(r)
	})
	return c
}

func TestFirebaseTerminalVerdictsDoNotFallbackAndClassifyAsReauth(t *testing.T) {
	for _, verdict := range []string{"TOKEN_EXPIRED", "INVALID_REFRESH_TOKEN", "MISSING_REFRESH_TOKEN", "USER_DISABLED", "USER_NOT_FOUND"} {
		for _, status := range []int{200, 400, 500} {
			t.Run(fmt.Sprintf("%s/%d", verdict, status), func(t *testing.T) {
				calls := 0
				c := officialAuthClient(t, func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.Host != "securetoken.googleapis.com" {
						t.Error("terminal verdict fell back to proxy")
					}
					return authResponse(status, `{"error":{"code":400,"message":"`+verdict+`"}}`), nil
				})
				_, err := c.refresh(t.Context(), Credentials{RefreshToken: "synthetic-refresh"})
				var failure *authFailure
				if !errors.As(err, &failure) || !failure.terminal() || failure.reason != verdict || calls != 1 {
					t.Fatalf("incorrect terminal verdict: %v calls=%d", err, calls)
				}
				if apperrors.ClassifyAccountStatus(err.Error()) != "401" || !accountpolicy.Classify(testAccount(), err, "").NeedsLogin {
					t.Fatal("terminal verdict did not reach account credential policy")
				}
			})
		}
	}
}

func TestFirebaseTransientFailuresUseOfficialProxy(t *testing.T) {
	for _, directFailure := range []string{"html", "transport", "firebase", "invalid_success"} {
		for _, custom := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/custom=%t", directFailure, custom), func(t *testing.T) {
				calls := 0
				c := officialAuthClient(t, func(r *http.Request) (*http.Response, error) {
					calls++
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					if custom {
						if r.Form.Get("token") != "synthetic-custom" || r.Form.Get("returnSecureToken") != "true" {
							t.Error("custom-token form changed")
						}
					} else if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "synthetic-refresh" {
						t.Error("refresh-token form changed")
					}
					if calls == 1 {
						if r.URL.Host == "app.warp.dev" {
							t.Error("did not try Firebase first")
						}
						switch directFailure {
						case "html":
							return authResponse(502, "<html>private-middlebox-body</html>"), nil
						case "transport":
							return nil, errors.New("private-transport-url-or-token")
						case "firebase":
							return authResponse(503, `{"error":{"message":"TEMPORARILY_UNAVAILABLE"}}`), nil
						default:
							return authResponse(200, `{"id_token":"synthetic","expires_in":"not-a-number"}`), nil
						}
					}
					wantPath := "/proxy/token"
					if custom {
						wantPath = "/proxy/customToken"
					}
					if calls != 2 || r.URL.Host != "app.warp.dev" || r.URL.Path != wantPath {
						t.Error("incorrect proxy fallback")
					}
					return authResponse(200, `{"idToken":"synthetic-new-access","refreshToken":"synthetic-new-refresh","expiresIn":"3600"}`), nil
				})
				var got Credentials
				var err error
				if custom {
					got, err = c.exchangeFirebase(t.Context(), "synthetic-custom")
				} else {
					got, err = c.refresh(t.Context(), Credentials{RefreshToken: "synthetic-refresh", Email: "user@example.invalid", UserID: "synthetic-uid"})
				}
				if err != nil || got.AccessToken != "synthetic-new-access" || got.RefreshToken != "synthetic-new-refresh" || calls != 2 {
					t.Fatalf("fallback failed: %v calls=%d", err, calls)
				}
				if !custom && (got.Email != "user@example.invalid" || got.UserID != "synthetic-uid") {
					t.Error("refresh lost identity metadata")
				}
			})
		}
	}
}

func TestFirebaseProxyTransientStatusHasRetryAfterAndNoReauth(t *testing.T) {
	for _, status := range []int{408, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			c := officialAuthClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return nil, errors.New("private-direct-error")
				}
				response := authResponse(status, "private-proxy-body")
				response.Header.Set("Retry-After", "300")
				return response, nil
			})
			_, err := c.refresh(t.Context(), Credentials{RefreshToken: "synthetic-refresh"})
			var failure *authFailure
			if !errors.As(err, &failure) || failure.terminal() || failure.RetryAfter() != 5*time.Minute || calls != 2 {
				t.Fatalf("unexpected transient verdict: %v", err)
			}
			if accountpolicy.Classify(testAccount(), err, "").NeedsLogin || strings.Contains(err.Error(), "private-") {
				t.Fatal("temporary failure poisoned credentials or leaked upstream data")
			}
		})
	}
}

func TestAuthRetryAfterFormats(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"0", time.Second}, {"30", 30 * time.Second}, {"9999999999999999999", 5 * time.Minute}, {"-1", 0}, {"bad", 0},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second}, {now.Add(-time.Hour).Format(http.TimeFormat), 0},
	} {
		if got := authRetryAfter(tc.value, now); got != tc.want {
			t.Errorf("Retry-After %q: got %v want %v", tc.value, got, tc.want)
		}
	}
}

func TestFirebaseCustomEndpointsNeverUseOfficialFallback(t *testing.T) {
	for _, field := range []string{"base", "firebase", "token"} {
		t.Run(field, func(t *testing.T) {
			calls := 0
			c := officialAuthClient(t, func(r *http.Request) (*http.Response, error) { calls++; return authResponse(503, `{}`), nil })
			switch field {
			case "base":
				c.base = "https://custom.invalid"
			case "firebase":
				c.firebaseBase = "https://custom.invalid"
			case "token":
				c.tokenBase = "https://securetoken.googleapis.com/custom"
			}
			_, err := c.refresh(t.Context(), Credentials{RefreshToken: "synthetic-refresh"})
			if err == nil || calls != 1 {
				t.Fatal("custom endpoint credentials reached official fallback")
			}
		})
	}
}

func TestFirebaseErrorsAreRedactedAndRedirectsAreNotFollowed(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{302, `{}`}, {200, `{"id_token":"synthetic","idToken":"conflicting","expires_in":"3600"}`},
		{400, `{"error":{"message":"INVALID_REFRESH_TOKEN private-token"}}`}, {200, strings.Repeat("private-body", 220000)},
	} {
		calls := 0
		c := officialAuthClient(t, func(r *http.Request) (*http.Response, error) {
			calls++
			response := authResponse(tc.status, tc.body)
			response.Header.Set("Location", "https://private.invalid/credential")
			return response, nil
		})
		_, err := c.refresh(t.Context(), Credentials{RefreshToken: "private-refresh"})
		var failure *authFailure
		if !errors.As(err, &failure) || failure.terminal() || calls != 2 || strings.Contains(err.Error(), "private") || errors.Unwrap(err) != nil {
			t.Fatalf("unsafe auth failure: calls=%d err=%v", calls, err)
		}
	}
}

func TestFirebaseCancellationDoesNotFallbackOrCache(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	c := officialAuthClient(t, func(r *http.Request) (*http.Response, error) { calls++; cancel(); return nil, context.Canceled })
	_, err := c.refresh(ctx, Credentials{RefreshToken: "synthetic-refresh"})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("cancellation triggered proxy fallback")
	}
	_, err = c.refresh(ctx, Credentials{RefreshToken: "synthetic-refresh"})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("canceled exchange reached network")
	}
	key := c.authFailureKey(credentials(c.account))
	rememberAuthFailure(key, err, time.Now())
	if cachedAuthFailure(key, time.Now()) != nil {
		t.Fatal("caller cancellation became an auth failure")
	}
}
