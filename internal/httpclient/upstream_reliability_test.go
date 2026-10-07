package httpclient

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type auditReadBody struct{}

func (auditReadBody) Read(p []byte) (int, error) { return copy(p, []byte("{}")), io.ErrUnexpectedEOF }
func (auditReadBody) Close() error               { return nil }

type auditBodyTransport struct{}

func (auditBodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: auditReadBody{}}, nil
}
func TestUpstreamReliabilityControlPlaneReadFailureIsReported(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://audit.invalid", strings.NewReader(""))
	_, _, err := DoReadBody(&http.Client{Transport: auditBodyTransport{}}, req, 1024)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("response body read error swallowed: got %v want unexpected EOF", err)
	}
}

type oversizedControlPlane struct{}

func (oversizedControlPlane) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("12345"))}, nil
}
func TestControlPlaneResponseOversizeIsExplicit(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://mock.invalid", nil)
	resp, raw, err := DoReadBody(&http.Client{Transport: oversizedControlPlane{}}, req, 4)
	if resp == nil || string(raw) != "1234" || !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("response=%v bytes=%q err=%v", resp, raw, err)
	}
}
