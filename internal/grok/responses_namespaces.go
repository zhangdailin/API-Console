package grok

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"orchids-api/internal/responses"
)

func restoreNativeResourceNamespaces(resp *http.Response, stored json.RawMessage) error {
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
	if namespaces.RestoreEnvelope(envelope) {
		raw, err = json.Marshal(envelope)
		if err != nil {
			return err
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return nil
}
