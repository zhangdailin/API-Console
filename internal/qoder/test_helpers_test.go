package qoder

import (
	"fmt"
	"io"
	"strings"
)

func setTestEndpoints(c *Client, oauth, openAPI, inference string) {
	if c == nil {
		return
	}
	c.endpoints.oauth = strings.TrimRight(oauth, "/")
	c.endpoints.openAPI = strings.TrimRight(openAPI, "/")
	c.endpoints.inference = strings.TrimRight(inference, "/")
	// A stub server must never fail over to the real failover nodes.
	c.endpoints.inferenceFallbacks = nil
}

func setTestEntropy(c *Client, reader io.Reader) {
	if c == nil {
		return
	}
	if reader == nil {
		c.entropy = cryptoSource{}
		return
	}
	c.entropy = testReaderSource{reader: reader}
}

type testReaderSource struct{ reader io.Reader }

func (r testReaderSource) Read(p []byte) (int, error) {
	n, err := io.ReadFull(r.reader, p)
	if err != nil {
		return n, fmt.Errorf("test entropy source: %w", err)
	}
	return n, nil
}

func decodeBodyForTest(encoded []byte) ([]byte, error) {
	if strings.ContainsAny(string(encoded), "\r\n") {
		return nil, fmt.Errorf("encoded body contains a line break")
	}
	decoded := make([]byte, bodyEncoding.DecodedLen(len(encoded)))
	n, err := bodyEncoding.Decode(decoded, swapOuterThirds(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	return decoded[:n], nil
}
