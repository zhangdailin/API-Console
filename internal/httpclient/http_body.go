package httpclient

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

// DoReadBody executes a non-streaming control-plane request, reads at most limit
// bytes and closes the body. Partial bytes and the HTTP response are retained
// for status classification, but incomplete/oversized bodies are never success.
func DoReadBody(client *http.Client, req *http.Request, limit int64) (*http.Response, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(raw)) > limit {
		return resp, raw[:limit], errors.Join(ErrBodyTooLarge, readErr)
	}
	if readErr != nil {
		return resp, raw, fmt.Errorf("read control-plane response: %w", readErr)
	}
	return resp, raw, nil
}

var ErrBodyTooLarge = errors.New("control-plane response exceeds size limit")
