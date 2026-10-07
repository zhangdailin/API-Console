package qoder

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"orchids-api/internal/httpclient"
	"strings"

	"encoding/json"

	"orchids-api/internal/util"
)

// The model catalog is a control-plane read on the same COSY-signed surface as
// the chat call. It is therefore reachable with the credential this channel
// already holds: the signature is computed over the runtime key, the encoded
// body and the signed path, none of which are chat specific.
//
// The observed catalog read uses this GET route and has no request body.
const modelListPath = "/algo/api/v2/model/list?Encode=1"

// FetchUpstreamModels reads the account-scoped model catalog from the signed
// upstream control plane.
//
// There is no local fallback. A compiled-in catalog is not an observation of
// what the account may run, so a failed read is reported as a failure; the
// caller decides whether that is worth surfacing.
func (c *Client) FetchUpstreamModels(ctx context.Context) (*Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("qoder client is nil")
	}
	creds, err := c.ensureAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	fields, err := c.ensureRuntimeFields(ctx, creds)
	if err != nil {
		return nil, err
	}

	return c.fetchModelListOnce(ctx, creds, fields)
}

// fetchModelListOnce performs one signed catalog read.
func (c *Client) fetchModelListOnce(ctx context.Context, creds Credentials, fields RuntimeFields) (*Catalog, error) {
	url := strings.TrimRight(c.endpoints.inference, "/") + modelListPath

	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	requestID, err := newUUID(c.entropy)
	if err != nil {
		return nil, err
	}
	if err := c.applyAuthHeaders(req, creds, fields, requestID, "", "", "", signPath(url)); err != nil {
		return nil, err
	}
	// The catalog is a JSON document, not an event stream: overriding the chat
	// path's Accept header keeps a strict gateway from wrapping the reply.
	req.Header.Set("Accept", "application/json")

	resp, raw, err := httpclient.DoReadBody(c.control, req, 4<<20)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAuthUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(http.MethodGet, url, resp.StatusCode, raw)
	}

	catalog, parseErr := parseModelList(raw)
	if parseErr != nil {
		// The gateway's own reason travels in the error; the raw body is not
		// reported upwards so an unrelated payload cannot reach a log line.
		return nil, fmt.Errorf("%s %s: %w", http.MethodGet, signPath(url), parseErr)
	}
	if catalog.Len() == 0 {
		return nil, fmt.Errorf("%s %s returned an empty catalog", http.MethodGet, signPath(url))
	}
	return catalog, nil
}

// parseModelList accepts the observed chat group and its Encode=1 data wrapper.
func parseModelList(raw []byte) (*Catalog, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty response")
	}

	if entries, ok := decodeCatalogEntries(trimmed); ok {
		return newCatalog(entries), nil
	}

	// A failure envelope is worth reporting verbatim: it carries the gateway's
	// own reason, which the caller surfaces instead of a generic parse error.
	var failure struct {
		Message string `json:"message"`
		Msg     string `json:"msg"`
		MsgInfo string `json:"msgInfo"`
	}
	if err := json.Unmarshal(trimmed, &failure); err == nil {
		if detail := util.FirstNonEmpty(failure.Message, failure.MsgInfo, failure.Msg); detail != "" {
			return nil, fmt.Errorf("upstream reported: %s", detail)
		}
	}
	return nil, fmt.Errorf("catalog response carried no model list")
}

// Only chat rows carrying a key can establish a catalog observation. Unwrap
// data and encoded chat once each; arbitrary groups and recursive nesting are
// not part of the observed wire contract.
func decodeCatalogEntries(raw json.RawMessage) ([]modelEntry, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false
	}
	if data, ok := fields["data"]; ok {
		fields = nil
		if err := json.Unmarshal(decodeCatalogString(data), &fields); err != nil {
			return nil, false
		}
	}
	var entries []modelEntry
	if err := json.Unmarshal(decodeCatalogString(fields["chat"]), &entries); err != nil {
		return nil, false
	}
	return usableCatalogEntries(entries)
}

func decodeCatalogString(raw json.RawMessage) json.RawMessage {
	var inner string
	if json.Unmarshal(raw, &inner) == nil {
		return json.RawMessage(inner)
	}
	return raw
}

// usableCatalogEntries reports the decodable rows that carry a key, and whether
// any did.
func usableCatalogEntries(entries []modelEntry) ([]modelEntry, bool) {
	usable := make([]modelEntry, 0, len(entries))
	for _, entry := range entries {
		if strings.TrimSpace(entry.Key) == "" {
			continue
		}
		usable = append(usable, entry)
	}
	return usable, len(usable) > 0
}
