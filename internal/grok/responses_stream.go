package grok

import (
	"io"
	"net/http"

	"orchids-api/internal/responses"
	"orchids-api/internal/util"
)

// The chat-to-Responses stream translation is protocol work and now lives in
// internal/responses. grok supplies the one part that is not: the private
// x_grok_search delta its own chat layer injects.

// chatStreamOptions configures one translation.
type chatStreamOptions = responses.StreamOptions

// grokSearchHook turns a Grok Build private search delta into a Responses
// output item. x_grok_search is not an OpenAI field: it is injected by this
// gateway's Build-to-chat layer, so the knowledge stays here.
func grokSearchHook(delta map[string]interface{}) (string, map[string]interface{}, bool) {
	search, ok := delta["x_grok_search"].(map[string]interface{})
	if !ok {
		return "", nil, false
	}
	value := responses.CloneStringInterfaceMap(search)
	if responses.ParseLooseStringAny(value["id"]) == "" {
		value["id"] = "ws_" + util.RandomHex(12)
	}
	done, _ := delta["x_grok_search_done"].(bool)
	value["status"] = "in_progress"
	if done {
		value["status"] = util.FirstNonEmpty(responses.ParseLooseStringAny(search["status"]), "completed")
	}
	return searchIdentity(search), value, done
}

// writeResponsesStreamFromChatReaderRequest translates a chat stream and always
// registers the Build search hook: every Grok stream can carry one.
func writeResponsesStreamFromChatReaderRequest(w http.ResponseWriter, request responses.CreateRequest, reader io.Reader, opts chatStreamOptions) {
	opts.SearchHook = grokSearchHook
	responses.WriteStreamFromChatReader(w, request, reader, opts)
}

// writeResponsesStreamFromChatReaderRequestWithHook is the test-facing form.
func writeResponsesStreamFromChatReaderRequestWithHook(w http.ResponseWriter, request responses.CreateRequest, reader io.Reader, onComplete func(map[string]interface{})) {
	writeResponsesStreamFromChatReaderRequest(w, request, reader, chatStreamOptions{OnComplete: onComplete})
}
