package grok

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"orchids-api/internal/responses"
)

func restoreNativeResourceNamespaces(resp *http.Response, stored json.RawMessage, hideReasoning ...bool) error {
	namespaces := responses.ToolNamespaces{}
	if err := namespaces.MergeStored(stored); err != nil {
		return err
	}
	raw, err := readAndValidateNativeResponse(resp.Body)
	if err != nil {
		return err
	}
	var envelope map[string]interface{}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	changed := namespaces.RestoreEnvelope(envelope)
	if len(hideReasoning) > 0 && hideReasoning[0] {
		changed = hideNativeReasoning(envelope) || changed
	}
	if changed {
		raw, err = json.Marshal(envelope)
		if err != nil {
			return err
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return nil
}
