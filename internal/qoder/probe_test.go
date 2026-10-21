package qoder

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"orchids-api/internal/testutil"
)

// TestProbeIdentitiesReportsEachQueue drives the probe against a stub gateway
// that serves only the CLI identity and refuses every other one with the p4
// queue payload production returned.
func TestProbeIdentitiesReportsEachQueue(t *testing.T) {
	const p4 = `{"code":"10605","message":"{\"isQueued\":true,\"modelKey\":\"qfmodel\",\"queueCount\":0,\"queueType\":\"p4\",\"retryAfterSeconds\":30,\"serviceAvailable\":false,\"subCode\":\"RESOURCE_LIMIT\",\"waitTime\":30}"}`
	var mu sync.Mutex
	seen := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoded, _ := io.ReadAll(r.Body)
		raw, err := decodeBody(encoded)
		testutil.NoError(t, err)
		var body map[string]any
		testutil.NoError(t, json.Unmarshal(raw, &body))
		// The COSY payload must carry the version the variant presents.
		payload := strings.SplitN(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer COSY."), ".", 2)[0]
		decoded, _ := base64.StdEncoding.DecodeString(payload)
		var cosy struct {
			CosyVersion string `json:"cosyVersion"`
		}
		_ = json.Unmarshal(decoded, &cosy)
		mu.Lock()
		seen[body["session_type"].(string)+"/"+r.Header.Get("Cosy-Clienttype")+"/"+r.Header.Get("Cosy-Version")] = cosy.CosyVersion + "|" + r.Header.Get("Cosy-Version")
		mu.Unlock()
		if body["session_type"] == "qodercli" {
			_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`)+"event:finish\ndata: {}\n\n")
			return
		}
		_, _ = io.WriteString(w, "data:"+mustJSON(map[string]any{"body": p4, "statusCodeValue": 403})+"\n\n")
	}))
	defer server.Close()

	acc := signedTestAccount()
	acc.QoderQuota.UserType = "personal_professional_trial"
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)
	results, err := client.ProbeIdentities(context.Background(), "Qwen3.7-Max")
	testutil.NoError(t, err)
	testutil.Equal(t, len(results), 8)

	byName := map[string]ProbeResult{}
	for _, r := range results {
		byName[r.Variant[:1]] = r
	}
	testutil.Equal(t, byName["A"].Outcome, "queued")
	testutil.Equal(t, byName["A"].QueueType, "p4")
	testutil.Equal(t, byName["A"].Available, "false")
	testutil.Equal(t, byName["A"].SubCode, "RESOURCE_LIMIT")
	testutil.Equal(t, byName["A"].RetryWait, "30")
	testutil.Equal(t, byName["E"].Outcome, "served")

	// Each identity reached the gateway as itself, signed with its own version.
	testutil.Equal(t, seen["qoder_work/6/"+DefaultClientVersion], DefaultClientVersion+"|"+DefaultClientVersion)
	testutil.Equal(t, seen["qoder/5/1.0.10"], "1.0.10|1.0.10")
	testutil.Equal(t, seen["qodercli/5/0.1.43"], "0.1.43|0.1.43")
	testutil.Equal(t, seen["qodercli/5/1.1.64"], "1.1.64|1.1.64")
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// TestProbeRoundsAlternatesOrder pins the fairness rule: a gateway that admits
// the first request after a pause favours whichever identity is sent first, so
// a repeated comparison flips the order every round.
func TestProbeRoundsAlternatesOrder(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoded, _ := io.ReadAll(r.Body)
		raw, _ := decodeBody(encoded)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		sent = append(sent, body["session_type"].(string))
		mu.Unlock()
		_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`)+"event:finish\ndata: {}\n\n")
	}))
	defer server.Close()

	client := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)
	var progress int
	results, err := client.ProbeRounds(context.Background(), "Qwen3.7-Max", ProbeRoundOptions{
		Variants: []string{"A", "e"}, Rounds: 3,
		Progress: func(int, ProbeResult) { progress++ },
	})
	testutil.NoError(t, err)
	testutil.Equal(t, len(results), 6)
	testutil.Equal(t, progress, 6)
	testutil.Equal(t, strings.Join(sent, ","), "qoder_work,qodercli,qodercli,qoder_work,qoder_work,qodercli")

	_, err = client.ProbeRounds(context.Background(), "Qwen3.7-Max", ProbeRoundOptions{Variants: []string{"Z"}})
	testutil.Error(t, err)
}

// TestProbeIdentitiesCoverEveryFailoverNode: api3 was never probed because the
// list stopped at the first failover node.
func TestProbeIdentitiesCoverEveryFailoverNode(t *testing.T) {
	client := NewFromAccount(signedTestAccount(), nil)
	identities := client.probeIdentities("personal_standard")
	hosts := map[string]string{}
	for _, identity := range identities {
		hosts[identity.name[:1]] = hostName(identity.host)
	}
	testutil.Equal(t, hosts["A"], "api2.qoder.sh")
	testutil.Equal(t, hosts["F"], "api1.qoder.sh")
	testutil.Equal(t, hosts["G"], "api1.qoder.sh")
	testutil.Equal(t, hosts["J"], "api3.qoder.sh")
	testutil.Equal(t, hosts["K"], "api3.qoder.sh")
	selected, err := selectProbeIdentities(identities, []string{"A", "F", "J"})
	testutil.NoError(t, err)
	testutil.Equal(t, len(selected), 3)
}
