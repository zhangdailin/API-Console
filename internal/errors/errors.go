// Package errors provides the shared error-handling mechanism.
package errors

import (
	"maps"
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
	// Details are extra machine-readable fields published inside the error
	// object. They are additive: a client that ignores them sees the same
	// envelope as before, and a client that reads them can tell a queue the
	// caller can wait out from one it should route around.
	Details map[string]any `json:"-"`
}

// ToJSON returns the JSON representation of the error.
func (e *AppError) ToJSON() []byte {
	detail := make(map[string]any, 2+len(e.Details))
	detail["type"] = e.Code
	detail["message"] = e.Message
	for key, value := range e.Details {
		if _, taken := detail[key]; taken {
			continue
		}
		detail[key] = value
	}
	data, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": detail,
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
// WithDetails attaches machine-readable fields to the error object.
func (e *AppError) WithDetails(details map[string]any) *AppError {
	if e == nil || len(details) == 0 {
		return e
	}
	if e.Details == nil {
		e.Details = make(map[string]any, len(details))
	}
	maps.Copy(e.Details, details)
	return e
}

func NewWithRetryAfter(code, message string, httpStatus int, retryAfter time.Duration) *AppError {
	err := New(code, message, httpStatus)
	if retryAfter > 0 {
		err.RetryAfter = retryAfter
	}
	return err
}
