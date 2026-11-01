package handler

import (
	"testing"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/testutil"
)

func TestClassifyAccountStatus(t *testing.T) {
	tests := []struct {
		name     string
		errStr   string
		expected string
	}{
		{
			name:     "Explicit 401",
			errStr:   "HTTP 401 Unauthorized",
			expected: "401",
		},
		{
			name:     "Explicit 403",
			errStr:   "HTTP 403 Forbidden",
			expected: "403",
		},
		{
			name:     "Model not found should not mark account",
			errStr:   "grok upstream status=403 body={\"error\":{\"code\":7,\"message\":\"Model is not found\",\"details\":[]}}",
			expected: "",
		},
		{
			name:     "Explicit 404",
			errStr:   "HTTP 404 Not Found",
			expected: "404",
		},
		{
			name:     "Signed out message",
			errStr:   "User is signed out",
			expected: "401",
		},
		{
			name:     "No active Clerk sessions",
			errStr:   "no active sessions found",
			expected: "401",
		},
		{
			name:     "Missing provider token",
			errStr:   "signed out: missing auth token",
			expected: "401",
		},
		{
			name:     "Forbidden message",
			errStr:   "Access forbidden",
			expected: "403",
		},
		{
			name:     "Explicit 429",
			errStr:   "HTTP 429 Too Many Requests",
			expected: "429",
		},
		{
			name:     "WorkBuddy insufficient funds maps to cooldown",
			errStr:   "workbuddy API error: code=insufficient_funds, status=402, message=Available funding is insufficient for this request.",
			expected: "402",
		},
		{
			name:     "Quota exceeded message",
			errStr:   "No remaining quota: No AI requests remaining",
			expected: "429",
		},
		{
			name:     "Rate limit message",
			errStr:   "Rate limit exceeded",
			expected: "429",
		},
		{
			name:     "Credits exhausted message",
			errStr:   "You have run out of credits. Please upgrade your plan to continue.",
			expected: "402",
		},
		{
			name:     "Server error (ignored)",
			errStr:   "HTTP 500 Internal Server Error",
			expected: "",
		},
		{
			name:     "Unknown error (ignored)",
			errStr:   "Something went wrong",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := apperrors.ClassifyAccountStatus(tt.errStr)
			testutil.CheckEqual(t, got, tt.expected)
		})
	}
}
