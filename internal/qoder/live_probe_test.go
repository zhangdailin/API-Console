//go:build live

package qoder

// Live probe (manual, opt-in). It requires -tags live and QODER_PROBE=1, so
// the normal test suite never talks to the upstream.
//
// It replays the production request construction against the real Qoder
// gateway from an account loaded out of the deployment's own Redis, one variant
// at a time, and prints only redacted evidence.
//
// Run on the deployment host:
//
//	cd /opt/orchids-2api/qoder-probe && QODER_PROBE=1 go test -tags live ./internal/qoder/ -run TestLiveProbe -v -count=1 -timeout 20m
//
// SECRETS: tokens, the COSY payload, the RSA key and the COSY signature are
// never printed. Only header names, the literal machine identity, status codes
// and the upstream's refusal metadata reach stdout.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"orchids-api/internal/httpclient"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"

	"golang.org/x/net/http2"
)

type probeVariant struct {
	label string
	apply func(req *http.Request)
	// transport selects the dialer: "" keeps Go's default (which, with an
	// explicit TLSClientConfig and no ForceAttemptHTTP2, speaks HTTP/1.1) and
	// "http2" forces the protocol the reference client negotiates.
	transport string
}

// probeEnvironment loads the deployment config, applies the same Redis
// overrides the server holds, and returns a store over that Redis.
func probeEnvironment(t *testing.T) (*config.Config, *store.Store) {
	t.Helper()
	cfg, _, err := config.Load("/opt/orchids-2api/config.json")
	if err != nil {
		t.Skipf("no deployment config: %v", err)
	}
	key, _, err := config.LoadOrCreateCredentialEncryptionKey("/opt/orchids-2api/config.json", cfg)
	if err != nil {
		t.Skipf("no credential key: %v", err)
	}
	s, err := store.New(store.Options{
		RedisAddr:               cfg.RedisAddr,
		RedisPassword:           cfg.RedisPassword,
		RedisDB:                 cfg.RedisDB,
		RedisPrefix:             cfg.RedisPrefix,
		CredentialEncryptionKey: key,
	})
	if err != nil {
		t.Skipf("store unavailable: %v", err)
	}
	if raw, err := s.GetSetting(context.Background(), "config"); err == nil && strings.TrimSpace(raw) != "" {
		var override map[string]interface{}
		if json.Unmarshal([]byte(raw), &override) == nil {
			if v, ok := override["qoder_inference_base_url"].(string); ok && v != "" {
				cfg.QoderInferenceURL = v
			}
			if v, ok := override["qoder_client_version"].(string); ok && v != "" {
				cfg.QoderClientVersion = v
			}
		}
	}
	return cfg, s
}

func probeAccounts(t *testing.T, s *store.Store) []*store.Account {
	t.Helper()
	accounts, err := s.ListAccounts(context.Background())
	if err != nil {
		t.Skipf("account list unavailable: %v", err)
	}
	includeExhausted := os.Getenv("QODER_PROBE_INCLUDE_EXHAUSTED") == "1"
	var usable []*store.Account
	for _, acc := range accounts {
		if !strings.EqualFold(strings.TrimSpace(acc.AccountType), "qoder") || !acc.Enabled {
			continue
		}
		if strings.TrimSpace(acc.QoderAccessToken) == "" {
			continue
		}
		if acc.QoderQuota.Exhausted && !includeExhausted {
			continue
		}
		usable = append(usable, acc)
	}
	sort.Slice(usable, func(i, j int) bool { return usable[i].RequestCount < usable[j].RequestCount })
	return usable
}

func TestLiveProbe(t *testing.T) {
	if os.Getenv("QODER_PROBE") != "1" {
		t.Skip("set QODER_PROBE=1 to talk to the real gateway")
	}
	cfg, s := probeEnvironment(t)
	accounts := probeAccounts(t, s)
	testutil.NotEqual(t, len(accounts), 0)
	if want := strings.TrimSpace(os.Getenv("QODER_PROBE_ACCOUNT")); want != "" {
		filtered := accounts[:0]
		for _, candidate := range accounts {
			if fmt.Sprint(candidate.ID) == want {
				filtered = append(filtered, candidate)
			}
		}
		testutil.NotEqual(t, len(filtered), 0)
		accounts = filtered
	}
	acc := accounts[0]
	client := NewFromAccount(acc, cfg)
	creds := ResolveCredentials(acc)
	testutil.NoError(t, client.PrepareCurrentRuntimeFields(context.Background()), "PROBE runtime prepare: %v")
	fields := client.RuntimeFields()
	testutil.Falsef(t, strings.TrimSpace(fields.Key) == "" || strings.TrimSpace(fields.EncryptUserInfo) == "", "PROBE runtime fields unavailable for account %d", acc.ID)
	model, err := client.resolveModel(upstream.UpstreamRequest{Model: "qwen3.8-flash"})
	testutil.NoError(t, err, "PROBE model resolve: %v")
	url := chatURL(client.endpoints.inference)

	// Optional: dump the exact decoded request body for a byte-level comparison
	// with a reference capture. It contains no credentials, only the caller's
	// prompt, so it stays opt-in.
	if out := strings.TrimSpace(os.Getenv("QODER_PROBE_BODY_OUT")); out != "" {
		requestID := fmt.Sprintf("probe-%d", time.Now().UnixNano())
		raw, err := buildChatBodyProfile(upstream.UpstreamRequest{
			Model:         "qwen3.8-flash",
			Messages:      []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "Reply exactly OK."}}},
			Prompt:        "Reply exactly OK.",
			RequestID:     requestID,
			ChatSessionID: "probe-session",
		}, model, "probe-session", requestID, requestID, client.clientVersion, client.aliyunUserType(), client.businessProduct())
		testutil.NoError(t, err, "PROBE body dump: %v")
		testutil.NoError(t, os.WriteFile(out, raw, 0o600), "PROBE body dump write: %v")
		fmt.Printf("PROBE body_dump=%s bytes=%d\n", out, len(raw))
	}

	fmt.Printf("PROBE account=%d model_key=%s source=%s machine_id=%s machine_token_len=%d machine_type=%s base=%s\n",
		acc.ID, model.Key, model.Source, client.machineID, len(client.machineToken), client.machineType,
		client.endpoints.inference)

	// The protocol question is the only one that has already separated a success
	// from a refusal, so the default matrix isolates it: the default Go client
	// (which, given an explicit TLSClientConfig, negotiates HTTP/1.1), a client
	// forced to HTTP/2, and the deployment's own shared transport.
	variants := []probeVariant{
		{label: "our-shared-transport", transport: "shared"},
		{label: "http2-explicit", transport: "http2"},
		{label: "default-client", transport: "default"},
	}

	if only := strings.TrimSpace(os.Getenv("QODER_PROBE_VARIANTS")); only != "" {
		wanted := map[string]bool{}
		for _, label := range strings.Split(only, ",") {
			wanted[strings.TrimSpace(label)] = true
		}
		kept := variants[:0]
		for _, variant := range variants {
			if wanted[variant.label] {
				kept = append(kept, variant)
			}
		}
		variants = kept
	}

	for i, variant := range variants {
		if i > 0 {
			time.Sleep(32 * time.Second)
		}
		runProbe(t, client, creds, fields, model, url, variant)
	}
}

func runProbe(t *testing.T, client *Client, creds Credentials, fields RuntimeFields, model modelEntry, url string, variant probeVariant) {
	t.Helper()
	requestID := fmt.Sprintf("probe-%d", time.Now().UnixNano())
	body, err := buildChatBodyProfile(upstream.UpstreamRequest{
		Model:         "qwen3.8-flash",
		Messages:      []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "Reply exactly OK."}}},
		Prompt:        "Reply exactly OK.",
		RequestID:     requestID,
		ChatSessionID: "probe-session",
	}, model, "probe-session", requestID, requestID, client.clientVersion, client.aliyunUserType(), client.businessProduct())
	if err != nil {
		fmt.Printf("PROBE variant=%s build_error=%v\n", variant.label, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		fmt.Printf("PROBE variant=%s new_request_error=%v\n", variant.label, err)
		return
	}
	if err := client.applyAuthHeaders(req, creds, fields, requestID, model.Key, model.Source, string(body), signPath(url)); err != nil {
		fmt.Printf("PROBE variant=%s auth_error=%v\n", variant.label, err)
		return
	}
	if variant.apply != nil {
		variant.apply(req)
	}

	start := time.Now()
	httpClient := &http.Client{Timeout: 75 * time.Second}
	switch variant.transport {
	case "http2":
		h2 := &http.Transport{TLSClientConfig: &tls.Config{}, ForceAttemptHTTP2: true}
		if err := http2.ConfigureTransport(h2); err != nil {
			fmt.Printf("PROBE variant=%s http2_setup_error=%v\n", variant.label, err)
			return
		}
		httpClient.Transport = h2
	case "shared":
		// Exactly the transport production uses for the chat stream, so the
		// probe answers for the deployed path rather than for a lookalike.
		httpClient = httpclient.GetSharedHTTPClient("qoder-probe", 75*time.Second, nil)
		httpClient.Timeout = 75 * time.Second
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Printf("PROBE variant=%s transport_error=%v elapsed=%.1fs\n", variant.label, err, time.Since(start).Seconds())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	detail := extractBodyMessage(raw)
	if detail == "" {
		detail = strings.TrimSpace(string(raw))
	}
	if len(detail) > 400 {
		detail = detail[:400]
	}
	fmt.Printf("PROBE variant=%s proto=%s status=%d elapsed=%.1fs code=%s detail=%s\n",
		variant.label, resp.Proto, resp.StatusCode, time.Since(start).Seconds(), envelopeCode(raw), detail)
}
