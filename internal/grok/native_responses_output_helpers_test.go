package grok

import (
	"io"
	"net/http"

	"orchids-api/internal/responses"
)

// copyNativeCLIResponseAndCaptureModel runs the production response copier
// (copyNativeResponseWithQuality) without a quality hold, which is how most
// tests drive it. It lives with the tests because only they call it.
func copyNativeCLIResponseAndCaptureModel(w http.ResponseWriter, body io.Reader, contentType, model string, mappings ...responses.ToolNamespaces) (responseID string, captured []byte, result chatOutcome) {
	var namespaces responses.ToolNamespaces
	if len(mappings) > 0 {
		namespaces = mappings[0]
	}
	return copyNativeResponseWithQuality(w, body, contentType, model, namespaces, nil)
}
