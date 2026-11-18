package warp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"orchids-api/internal/debug"
)

func TestDetailedRejectionReachesOnlyContentFreeDiagnostic(t *testing.T) {
	const private = "private-result-token-and-id"
	stream := integrityStream(t, compatRootTask("root"), integrityTaskMessages(officialServerResult(private, private, private)), testDone())
	client := NewFromAccount(testAccount(), testConfig("https://private.invalid"))
	client.http = &http.Client{Transport: diagnosticRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream)), Request: req}, nil
	})}
	ctx, capture := debug.WithCapture(context.Background(), "test-request")
	err := client.SendRequestWithPayload(ctx, testRequest(), nil, nil)
	if err == nil {
		t.Fatal("unpaired result accepted")
	}
	var found bool
	for _, section := range capture.Bundle().Sections {
		if section.Name != "3_warp_diagnostic.json" {
			continue
		}
		var d generationDiagnostic
		if json.Unmarshal([]byte(section.Payload), &d) != nil || d.Rejection == nil || d.Rejection.Reason != "server_unpaired" || d.Rejection.ResultKind == 0 || d.Rejection.EventOrdinal == 0 {
			t.Fatal("precise rejection lost before diagnostics")
		}
		if strings.Contains(section.Payload, private) || strings.Contains(section.Payload, "private.invalid") {
			t.Fatal("private content in diagnostic")
		}
		found = true
	}
	if !found {
		t.Fatal("missing diagnostic")
	}
	if strings.Contains(err.Error(), private) || errors.Is(err, errStreamIncomplete) {
		t.Fatal("unsafe public error")
	}
}
