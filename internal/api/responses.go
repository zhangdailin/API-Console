package api

import (
	"net/http"

	"orchids-api/internal/httpserver"
	"orchids-api/internal/util"
)

// JSON writers are shared with inference endpoints through internal/util.

// requireMethod rejects every method but the one the endpoint serves. It
// reports false when it has already written the 405, so the caller only has to
// return.
//
// The admin console answers 405 with the plain-text form (see
// httpserver.WritePlainMethodNotAllowed), which is why it does not use the
// OpenAI-envelope httpserver.RequireMethod the inference endpoints share.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	return httpserver.RequirePlainMethod(w, r, method)
}

// writeMethodNotAllowed answers the shared 405 body for the handlers that
// dispatch on the method themselves and so cannot use requireMethod.
func writeMethodNotAllowed(w http.ResponseWriter) {
	httpserver.WritePlainMethodNotAllowed(w)
}

// writeCodeEnvelope answers the {"code":...} envelope the console's settings
// endpoints share: 0 carries the payload, 1 carries an operator-facing message.
// Absent fields are left out rather than sent as null, so the shape a client
// sees is the shape it already parses.
func writeCodeEnvelope(w http.ResponseWriter, code int, data any, msg string) {
	payload := map[string]any{"code": code}
	if data != nil {
		payload["data"] = data
	}
	if msg != "" {
		payload["msg"] = msg
	}
	util.WriteJSON(w, payload)
}
