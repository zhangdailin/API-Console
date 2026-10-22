package qoder

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/testutil"
)

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// TestProbeReportsTheQueueRefusal parses the p4 refusal production returned.
func TestProbeReportsTheQueueRefusal(t *testing.T) {
	const p4 = `{"code":"10605","message":"{\"isQueued\":true,\"modelKey\":\"qfmodel\",\"queueCount\":0,\"queueType\":\"p4\",\"retryAfterSeconds\":30,\"serviceAvailable\":false,\"subCode\":\"RESOURCE_LIMIT\",\"waitTime\":30}"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data:"+mustJSON(map[string]any{"body": p4, "statusCodeValue": 403})+"\n\n")
	}))
	defer server.Close()
	client := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)
	results, err := client.ProbeRounds(context.Background(), "Qwen3.7-Max", ProbeRoundOptions{Rounds: 1})
	testutil.NoError(t, err)
	testutil.Equal(t, len(results), 1)
	r := results[0]
	testutil.Equal(t, r.Outcome, "queued")
	testutil.Equal(t, r.QueueType, "p4")
	testutil.Equal(t, r.Available, "false")
	testutil.Equal(t, r.SubCode, "RESOURCE_LIMIT")
	testutil.Equal(t, r.RetryWait, "30")
}

// TestProbeRoundsAlternatesOrder pins the fairness rule: a gateway that admits
// the first request after a pause favours whichever variant is sent first, so
// a repeated comparison flips the order every round.
func TestProbeRoundsAlternatesOrder(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	serve := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			sent = append(sent, name)
			mu.Unlock()
			_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`)+"event:finish\ndata: {}\n\n")
		}))
	}
	primary, fallback := serve("A"), serve("F")
	defer primary.Close()
	defer fallback.Close()

	client := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(client, primary.URL, primary.URL, primary.URL)
	client.endpoints.inferenceFallbacks = []string{fallback.URL}
	var progress int
	results, err := client.ProbeRounds(context.Background(), "Qwen3.7-Max", ProbeRoundOptions{
		Variants: []string{"A", "f"}, Rounds: 3,
		Progress: func(int, ProbeResult) { progress++ },
	})
	testutil.NoError(t, err)
	testutil.Equal(t, len(results), 6)
	testutil.Equal(t, progress, 6)
	testutil.Equal(t, strings.Join(sent, ","), "A,F,F,A,A,F")
	for _, r := range results {
		testutil.Equal(t, r.Outcome, "served")
	}

	_, err = client.ProbeRounds(context.Background(), "Qwen3.7-Max", ProbeRoundOptions{Variants: []string{"Z"}})
	testutil.Error(t, err)
}

// TestProbeVariantsCoverEveryNode: the daily request count is kept per host,
// so every failover node gets its own variant.
func TestProbeVariantsCoverEveryNode(t *testing.T) {
	client := NewFromAccount(signedTestAccount(), nil)
	hosts := map[string]string{}
	for _, variant := range client.probeVariants() {
		hosts[variant.name[:1]] = hostName(variant.host)
	}
	testutil.Equal(t, len(hosts), 3)
	testutil.Equal(t, hosts["A"], "api2.qoder.sh")
	testutil.Equal(t, hosts["F"], "api1.qoder.sh")
	testutil.Equal(t, hosts["J"], "api3.qoder.sh")
}

func TestUntilProbeWindow(t *testing.T) {
	at := func(seconds float64) time.Time {
		return time.Date(2026, 10, 21, 15, 1, 0, 0, time.UTC).Add(time.Duration(seconds * float64(time.Second)))
	}
	testutil.Equal(t, untilProbeWindow(at(0)), 9*time.Second)
	testutil.Equal(t, untilProbeWindow(at(8.5)), 500*time.Millisecond)
	testutil.Equal(t, untilProbeWindow(at(9)), time.Minute)
	testutil.Equal(t, untilProbeWindow(at(30)), 39*time.Second)
}
