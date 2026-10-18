//go:build live

package qoder

// Opt-in generation acceptance test. The production client constructs and
// consumes the request; a successful control-plane check is not acceptance.
//
// QODER_PROBE=1 QODER_PROBE_ACCOUNT=79 go test -tags live ./internal/qoder \
//   -run '^TestLiveProbe$' -v -count=1 -timeout 5m
//
// QODER_PROBE_CONFIG overrides the deployment config path. QODER_PROBE_MODEL
// selects an observed model. QODER_PROBE_VARIANTS optionally selects a comma-
// separated transport matrix: our-shared-transport,http2-explicit,default-client.
// Credentials remain on the deployment host. Each variant sends one request;
// expiring credentials must be refreshed by the running service first.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func probeEnvironment(t *testing.T) (*config.Config, *store.Store) {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("QODER_PROBE_CONFIG"))
	if path == "" {
		path = "/opt/orchids-2api/config.json"
	}
	cfg, resolved, err := config.Load(path)
	if err != nil {
		t.Fatal("PROBE deployment config unavailable")
	}
	key, _, err := config.LoadOrCreateCredentialEncryptionKey(resolved, cfg)
	if err != nil {
		t.Fatal("PROBE credential key unavailable")
	}
	s, err := store.New(store.Options{
		RedisAddr: cfg.RedisAddr, RedisPassword: cfg.RedisPassword,
		RedisDB: cfg.RedisDB, RedisPrefix: cfg.RedisPrefix, CredentialEncryptionKey: key,
	})
	if err != nil {
		t.Fatal("PROBE deployment store unavailable")
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := s.GetSetting(ctx, "config")
	if err != nil {
		t.Fatal("PROBE runtime config unavailable")
	}
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), cfg); err != nil {
			t.Fatal("PROBE runtime config invalid")
		}
	}
	return cfg, s
}

func TestLiveProbe(t *testing.T) {
	if os.Getenv("QODER_PROBE") != "1" {
		t.Skip("set QODER_PROBE=1 to verify real generation")
	}
	cfg, s := probeEnvironment(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	accounts, err := s.ListAccounts(ctx)
	cancel()
	if err != nil {
		t.Fatal("PROBE account list unavailable")
	}
	want := strings.TrimSpace(os.Getenv("QODER_PROBE_ACCOUNT"))
	includeExhausted := os.Getenv("QODER_PROBE_INCLUDE_EXHAUSTED") == "1"
	var usable []*store.Account
	for _, acc := range accounts {
		if acc.AccountType != "qoder" || !acc.Enabled || (want != "" && fmt.Sprint(acc.ID) != want) {
			continue
		}
		if !ResolveCredentials(acc).AccessValid(time.Now()) || (acc.QoderQuota.Exhausted && !includeExhausted) {
			continue
		}
		usable = append(usable, acc)
	}
	if len(usable) == 0 {
		t.Fatal("PROBE no matching account with a current credential; refresh through the service first")
	}
	sort.Slice(usable, func(i, j int) bool { return usable[i].RequestCount < usable[j].RequestCount })
	acc := usable[0]
	model := strings.TrimSpace(os.Getenv("QODER_PROBE_MODEL"))
	if model == "" {
		model = "qwen3.8-flash"
	}
	variants := []string{"our-shared-transport"}
	if selected := strings.TrimSpace(os.Getenv("QODER_PROBE_VARIANTS")); selected != "" {
		variants = strings.Split(selected, ",")
	}
	for _, variant := range variants {
		switch strings.TrimSpace(variant) {
		case "our-shared-transport", "http2-explicit", "default-client":
		default:
			t.Fatal("PROBE invalid transport variant")
		}
	}
	var nextProbe time.Time
	for _, selected := range variants {
		variant := strings.TrimSpace(selected)
		if delay := time.Until(nextProbe); delay > 0 {
			time.Sleep(delay)
		}
		t.Run(variant, func(t *testing.T) {
			client := NewFromAccount(acc, cfg)
			if variant != "our-shared-transport" {
				transport, ok := client.stream.Transport.(*http.Transport)
				if !ok {
					t.Fatal("PROBE cannot clone the production transport")
				}
				cloned := transport.Clone()
				cloned.ForceAttemptHTTP2 = variant == "http2-explicit"
				if cloned.TLSClientConfig != nil {
					cloned.TLSClientConfig = cloned.TLSClientConfig.Clone()
					cloned.TLSClientConfig.NextProtos = nil
				}
				copyClient := *client.stream
				copyClient.Transport = cloned
				client.stream = &copyClient
				t.Cleanup(cloned.CloseIdleConnections)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
			defer cancel()
			cap := 128
			result, err := probeGeneration(ctx, client, upstream.UpstreamRequest{
				Model: model, Prompt: "Reply exactly OK.", MaxTokens: &cap, ReasoningEffort: "none",
			})
			nextProbe = time.Now().Add(32 * time.Second)
			var hint interface{ RetryAfter() time.Duration }
			if errors.As(err, &hint) && hint.RetryAfter() > 32*time.Second {
				nextProbe = time.Now().Add(hint.RetryAfter())
			}
			// No upstream payload, credential, generated text or device identifier
			// is printed, even when an upstream rejection contains echoed data.
			t.Logf("PROBE account=%d model=%s proto=%s elapsed=%s output=%t finished=%t",
				acc.ID, model, result.protocol, result.elapsed, result.output, result.finished)
			if err != nil {
				category := apperrors.ClassifyUpstreamError(err.Error()).Category
				t.Fatalf("PROBE generation failed: category=%s", category)
			}
		})
	}
}

type probeOutcome struct {
	output, finished bool
	protocol         string
	elapsed          time.Duration
}

type probeTransport struct {
	base    http.RoundTripper
	outcome *probeOutcome
}

func (p probeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := p.base.RoundTrip(req)
	if response != nil {
		p.outcome.protocol = response.Proto
	}
	return response, err
}

// probeGeneration uses the public production entry point and its stream parser.
// One shared attempt prevents auth repair from rotating a live credential, and
// prevents transient retries from making a rejected sample look successful.
func probeGeneration(ctx context.Context, client *Client, req upstream.UpstreamRequest) (probeOutcome, error) {
	var result probeOutcome
	if !client.currentCredentials().AccessValid(time.Now()) {
		return result, errors.New("probe requires a current access token")
	}
	copyClient := *client.stream
	copyClient.Transport = probeTransport{base: copyClient.Transport, outcome: &result}
	client.stream = &copyClient
	ctx, _ = upstream.WithAttemptBudget(ctx, 1)
	start := time.Now()
	err := client.SendRequestWithPayload(ctx, req, func(message upstream.SSEMessage) {
		switch message.Type {
		case "model.text-delta":
			if delta, ok := message.Event["delta"].(string); ok && strings.TrimSpace(delta) != "" {
				result.output = true
			}
		case "model.tool-call":
			result.output = true
		case "model.finish":
			result.finished = true
		}
	}, nil)
	result.elapsed = time.Since(start)
	if err == nil && (!result.output || !result.finished) {
		err = errors.New("probe requires visible output and a complete stream")
	}
	return result, err
}
