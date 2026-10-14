// Package errors provides the shared error-handling mechanism.
package errors

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"encoding/json"
)

// AppError is an application-layer error: a code, a message and an optional cause.
type AppError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"-"`
	// RetryAfter, when positive, is published as the Retry-After header. A
	// capacity answer that took a minute of upstream retries to produce has to
	// tell the caller when to come back: without it the client only learns that
	// something failed and retries on its own schedule, which adds load to the
	// condition that produced the answer.
	RetryAfter time.Duration `json:"-"`
}

// ToJSON returns the JSON representation of the error.
func (e *AppError) ToJSON() []byte {
	data, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    e.Code,
			"message": e.Message,
		},
	})
	return data
}

// WriteResponse writes the error into the HTTP response.
func (e *AppError) WriteResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	if e.RetryAfter > 0 {
		seconds := max(int(math.Ceil(e.RetryAfter.Seconds())), 1)
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
	}
	w.WriteHeader(e.HTTPStatus)
	w.Write(e.ToJSON())
}

// New creates a new application error.
func New(code, message string, httpStatus int) *AppError {
	return &AppError{
		Code:       code,
		Message:    message,
		HTTPStatus: httpStatus,
	}
}

// NewWithRetryAfter is New plus the upstream's own "come back in" hint, which
// WriteResponse publishes as the Retry-After header.
func NewWithRetryAfter(code, message string, httpStatus int, retryAfter time.Duration) *AppError {
	err := New(code, message, httpStatus)
	if retryAfter > 0 {
		err.RetryAfter = retryAfter
	}
	return err
}
