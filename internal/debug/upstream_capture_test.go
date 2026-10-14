package debug

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
)

func TestUpstreamCaptureRetainsInterleavedAttempts(t *testing.T) {
	ctx, c := WithCapture(context.Background(), "retry")
	headers := http.Header{"Authorization": []string{"Bearer private-credential"}, "Cookie": []string{"session-private"}}
	a := BeginUpstream(ctx, "POST", "https://first.invalid", headers, []byte(`{"input":"first"}`))
	b := BeginUpstream(ctx, "POST", "https://second.invalid", nil, []byte(`{"input":"second"}`))
	a.Response(&http.Response{StatusCode: 429, Header: http.Header{}}, nil)
	b.Response(&http.Response{StatusCode: 200, Header: http.Header{}}, nil)
	// Response A arrives after B starts. It must still belong to A.
	for _, tc := range []struct {
		a    *UpstreamAttempt
		body string
	}{{b, "second response"}, {a, "first response"}} {
		got, err := io.ReadAll(tc.a.CaptureBody(io.NopCloser(strings.NewReader(tc.body))))
		testutil.False(t, err != nil || string(got) != tc.body, "tee changed data")
	}
	sections := map[string]string{}
	for _, s := range c.Bundle().Sections {
		sections[s.Name] = s.Payload
		testutil.MustNotContainAny(t, s.Payload, "private-credential", "session-private")
	}
	for _, tc := range []struct{ name, want string }{
		{"upstream_001_request.json", "first.invalid"}, {"upstream_002_request.json", "second.invalid"},
		{"upstream_001_response.txt", "first response"}, {"upstream_002_response.txt", "second response"},
		{"upstream_001_result.json", "429"}, {"upstream_002_result.json", "200"},
	} {
		testutil.MustContain(t, sections[tc.name], tc.want)
	}
}

// TestLoggerRetryRequestsAreNotOverwritten pins that a retry owns its own
// capture section: the second request may not overwrite the first, so a report
// can always be read back per attempt.
func TestLoggerRetryRequestsAreNotOverwritten(t *testing.T) {
	ctx, c := WithCapture(context.Background(), "logger")
	l := NewForContext(ctx, false, false)
	for i := 1; i <= 2; i++ {
		l.LogUpstreamRequest(fmt.Sprintf("https://attempt-%d.invalid", i), nil, map[string]int{"attempt": i})
	}
	b := c.Bundle()
	testutil.Equal(t, len(b.Sections), 2)
	byName := map[string]string{}
	for _, s := range b.Sections {
		byName[s.Name] = s.Payload
	}
	for i, name := range []string{"upstream_001_request.json", "upstream_002_request.json"} {
		want := fmt.Sprintf("attempt-%d.invalid", i+1)
		testutil.Falsef(t, !strings.Contains(byName[name], want), "%s = %q, want it to hold %q", name, byName[name], want)
	}
}

func TestCapturePreservesManySectionsAndLargeBundle(t *testing.T) {
	_, c := WithCapture(context.Background(), "limits")
	defer c.Close()
	for i := range 97 {
		c.Append(fmt.Sprintf("small-%d", i), "x")
	}
	testutil.False(t, c.Bundle().Truncated || len(c.Bundle().Sections) != 97, "silently discarded sections")
	c.Close()
	_, c = WithCapture(context.Background(), "bytes")
	defer c.Close()
	for i := range 32 {
		c.Append(fmt.Sprintf("large-%d", i), strings.Repeat("x", maxCaptureBytes))
	}
	b := c.Bundle()
	if b.Truncated || b.Bytes != 32*maxCaptureBytes {
		t.Fatal("unbounded or unmarked", b.Bytes, b.Truncated)
	}
}
