package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
)

// MaxJSONBodyBytes bounds the request body of every JSON endpoint: an unbounded
// Decode lets one client allocate arbitrary memory inside the gateway.
const MaxJSONBodyBytes = 32 << 20

// DecodeJSONBody decodes the request body into v and writes the standard error
// response on failure: 415 for a non-JSON content type, 413 for an oversized
// body, 408 for a request-body read timeout and 400 for malformed JSON.
func DecodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	return DecodeJSONBodyLimit(w, r, v, MaxJSONBodyBytes)
}

// DecodeJSONBodyLimit lets small public endpoints use a stricter allocation bound.
func DecodeJSONBodyLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if r == nil || r.Body == nil {
		WriteError(w, http.StatusBadRequest, "invalid request")
		return false
	}
	if raw := strings.TrimSpace(r.Header.Get("Content-Type")); raw != "" {
		mediaType, _, err := mime.ParseMediaType(raw)
		if err != nil || !strings.EqualFold(mediaType, "application/json") {
			WriteErrorCode(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
			return false
		}
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(v); err != nil {
		writeJSONBodyError(w, err)
		return false
	}
	// A request contains exactly one JSON value. Reading through EOF also
	// enforces the limit on whitespace or a second value after a valid prefix.
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		writeJSONBodyError(w, err)
		return false
	}
	return true
}

// ReadBoundedJSONBody reads a JSON request body under the shared limit. It
// writes the 413/408/400 response itself and returns an error so the caller only
// has to return: an unbounded io.ReadAll lets one client allocate arbitrary
// memory inside the gateway.
func ReadBoundedJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r == nil || r.Body == nil {
		WriteError(w, http.StatusBadRequest, "invalid json")
		return nil, errors.New("empty body")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxJSONBodyBytes))
	if err != nil {
		writeJSONBodyError(w, err)
		return nil, err
	}
	return body, nil
}

func writeJSONBodyError(w http.ResponseWriter, err error) {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		WriteErrorCode(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the configured limit")
		return
	}
	if timeout, ok := errors.AsType[net.Error](err); ok && timeout.Timeout() {
		WriteError(w, http.StatusRequestTimeout, "request body read timed out")
		return
	}
	WriteError(w, http.StatusBadRequest, "invalid json")
}
