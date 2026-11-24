package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"

	"orchids-api/internal/middleware"
)

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
	if limit > 0 {
		if raw, ok := middleware.BufferedRequestBody(r); ok {
			r.Body = http.NoBody
			if int64(len(raw)) > limit {
				writeJSONBodyError(w, &http.MaxBytesError{Limit: limit})
				return false
			}
			if err := json.Unmarshal(raw, v); err != nil {
				writeJSONBodyError(w, err)
				return false
			}
			return true
		}
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

// ReadJSONBodyLimit reads the complete body within limit and reports read errors.
func ReadJSONBodyLimit(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r == nil || r.Body == nil {
		WriteError(w, http.StatusBadRequest, "invalid json")
		return nil, errors.New("empty body")
	}
	if body, ok := middleware.BufferedRequestBody(r); ok {
		r.Body = http.NoBody
		if int64(len(body)) > limit {
			err := &http.MaxBytesError{Limit: limit}
			writeJSONBodyError(w, err)
			return nil, err
		}
		return body, nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
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
