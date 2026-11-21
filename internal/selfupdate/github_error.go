package selfupdate

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type releaseCheckError struct {
	Code    string
	Message string
	RetryAt time.Time
}

func (e *releaseCheckError) Error() string { return e.Message }

// Inspect bounded error metadata for classification, but never expose upstream
// bodies, URLs, tokens or arbitrary remote messages to the admin response.
func githubCheckFailure(res *http.Response, now time.Time) *releaseCheckError {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 64*1024)).Decode(&body)
	message := strings.ToLower(body.Message)
	status := res.StatusCode
	primary := strings.TrimSpace(res.Header.Get("X-RateLimit-Remaining")) == "0"
	rateLimited := status == http.StatusTooManyRequests || (status == http.StatusForbidden &&
		(primary || res.Header.Get("Retry-After") != "" || strings.Contains(message, "rate limit") || strings.Contains(message, "abuse detection")))
	failure := &releaseCheckError{Code: "github_http_error", Message: fmt.Sprintf("GitHub update check failed: HTTP %d.", status)}
	if rateLimited {
		failure.Code = "github_rate_limited"
		failure.Message = fmt.Sprintf("GitHub rate limit reached (HTTP %d); wait before checking again.", status)
		failure.RetryAt = now.Add(time.Minute)
		if retry := githubRetryAfter(res.Header.Get("Retry-After"), now); retry.After(failure.RetryAt) {
			failure.RetryAt = retry
		}
		if primary {
			if seconds, err := strconv.ParseInt(res.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
				reset := time.Unix(seconds, 0).UTC()
				if reset.After(failure.RetryAt) && !reset.After(now.Add(24*time.Hour)) {
					failure.RetryAt = reset
				}
			}
		}
		return failure
	}
	switch status {
	case http.StatusUnauthorized:
		failure.Code = "github_unauthorized"
		failure.Message = "GitHub authentication failed (HTTP 401); check ORCHIDS_UPDATE_GITHUB_TOKEN."
	case http.StatusForbidden:
		failure.Code = "github_forbidden"
		failure.Message = "GitHub denied the update check (HTTP 403); check access policy or token permissions."
	case http.StatusNotFound:
		failure.Code = "github_release_not_found"
		failure.Message = "No stable GitHub release is accessible for this repository (HTTP 404)."
	}
	return failure
}

func githubRetryAfter(raw string, now time.Time) time.Time {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && seconds > 0 && seconds <= 86400 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if date, err := http.ParseTime(raw); err == nil && date.After(now) && !date.After(now.Add(24*time.Hour)) {
		return date.UTC()
	}
	return time.Time{}
}
