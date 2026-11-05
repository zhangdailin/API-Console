package web

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/grok/web/egress"
	"orchids-api/internal/httpclient"
	"orchids-api/internal/util"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func newHTTPClient(cfg *config.Config, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	proxyKey := "direct"
	if cfg != nil {
		proxyKey = httpclient.GenerateProxyKeyFromConfig(cfg)
	}

	return httpclient.GetSharedBrowserHTTPClientWithHeaderTimeout(proxyKey, timeout, 0, proxyFunc)
}

func parseRetryAfter(raw string) time.Duration {
	return parseRetryAfterHeader(raw, time.Now())
}

func parseUpstreamStatus(err error) int {
	if err == nil {
		return 0
	}
	raw := err.Error()
	idx := strings.Index(raw, "status=")
	if idx < 0 {
		return 0
	}
	rest := raw[idx+len("status="):]
	n := 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	if n == 0 {
		return 0
	}
	code, convErr := strconv.Atoi(rest[:n])
	if convErr != nil {
		return 0
	}
	return code
}

func backoffDelay(baseDelay, retry429Delay, lastDelay time.Duration, attempt int, status int, retryAfter time.Duration) time.Duration {
	maxDelay := 30 * time.Second
	if retryAfter > 0 {
		if retryAfter > maxDelay {
			return maxDelay
		}
		return retryAfter
	}

	if status == http.StatusTooManyRequests {
		if retry429Delay > 0 {
			if retry429Delay > maxDelay {
				return maxDelay
			}
			return retry429Delay
		}
		if lastDelay <= 0 {
			lastDelay = baseDelay
		}
		upper := lastDelay * 3
		if upper < baseDelay {
			upper = baseDelay
		}
		delay := baseDelay + time.Duration(rand.Int63n(int64(upper-baseDelay)+1))
		if delay > maxDelay {
			return maxDelay
		}
		return delay
	}

	if baseDelay <= 0 {
		baseDelay = 500 * time.Millisecond
	}
	exp := baseDelay * time.Duration(1<<max(0, attempt))
	if exp > maxDelay {
		exp = maxDelay
	}
	if exp <= 0 {
		return baseDelay
	}
	return time.Duration(rand.Int63n(int64(exp) + 1))
}

type responseBodyCloser struct {
	io.Reader
	closers []io.Closer
}

type closeFunc func() error

func (fn closeFunc) Close() error {
	return fn()
}

func (r responseBodyCloser) Close() error {
	var firstErr error
	for _, closer := range r.closers {
		if closer == nil {
			continue
		}
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func decodeHTTPResponseBody(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if idx := strings.Index(encoding, ","); idx >= 0 {
		encoding = strings.TrimSpace(encoding[:idx])
	}
	if encoding == "" || encoding == "identity" {
		return nil
	}

	original := resp.Body
	var (
		reader  io.Reader
		closers []io.Closer
	)
	switch encoding {
	case "gzip":
		gz, err := gzip.NewReader(original)
		if err != nil {
			return err
		}
		reader = gz
		closers = append(closers, gz)
	case "deflate":
		fr := flate.NewReader(original)
		reader = fr
		closers = append(closers, fr)
	case "br":
		reader = brotli.NewReader(original)
	case "zstd":
		zr, err := zstd.NewReader(original)
		if err != nil {
			return err
		}
		reader = zr
		closers = append(closers, closeFunc(func() error {
			zr.Close()
			return nil
		}))
	default:
		return nil
	}
	closers = append(closers, original)
	resp.Body = responseBodyCloser{Reader: reader, closers: closers}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return nil
}

func (c *Client) doRequest(ctx context.Context, reqURL string, method string, body []byte, headers http.Header, asset bool) (*http.Response, error) {
	return c.doRequestWithHTTPClient(ctx, c.clientForAsset(asset), reqURL, method, body, headers, http.StatusOK, true)
}

func (c *Client) doAppChatRequest(ctx context.Context, reqURL string, body []byte, headers http.Header) (*http.Response, error) {
	return c.doRequestWithHTTPClient(ctx, c.clientForAppChat(), reqURL, http.MethodPost, body, headers, http.StatusOK, false)
}

func appChatRateLimitRequest(body []byte, headers http.Header) (token, model string) {
	if headers != nil {
		token = NormalizeSSOToken(headers.Get("Cookie"))
	}
	if len(body) == 0 {
		return token, ""
	}
	var payload map[string]interface{}
	if json.Unmarshal(body, &payload) == nil {
		model = firstNonEmpty(parseLooseStringAny(payload["modelName"]), parseLooseStringAny(payload["model"]))
	}
	return token, model
}

func (c *Client) doRequestWithHTTPClient(ctx context.Context, httpClient *http.Client, reqURL string, method string, body []byte, headers http.Header, okStatus int, retry429 bool) (*http.Response, error) {
	if okStatus == 0 {
		okStatus = http.StatusOK
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	maxRetries := 0
	if c.cfg != nil && c.cfg.MaxRetries > 0 {
		maxRetries = c.cfg.MaxRetries
	}
	baseDelay := 1 * time.Second
	if c.cfg != nil && c.cfg.RetryDelay > 0 {
		baseDelay = time.Duration(c.cfg.RetryDelay) * time.Millisecond
	}
	retry429Delay := time.Duration(0)

	var lastStatus int
	var lastBody string
	lastDelay := baseDelay
	challengeRetried := false

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, reqURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		reqHeaders := cloneHeaderShallow(headers, 1)
		reqHeaders.Set("x-xai-request-id", randomUUID())
		req.Header = reqHeaders

		// Egress: route through the proxy pool with a bound UA + clearance. When
		// egress is explicitly enabled the manager is fail-closed: an acquire
		// failure is returned, never silently downgraded to the direct client.
		var resp *http.Response
		var lease *egress.Lease
		leaseNodeID := ""
		releaseLease := func() {}
		if scope := c.egressScopeForURL(reqURL); c.egress != nil && c.egress.Routes(scope) {
			affinity := requestCredentialAffinity(req.Header, reqURL)
			lease, err = c.egress.Acquire(ctx, scope, affinity)
			if err != nil {
				recordEgressAcquireError()
				return nil, fmt.Errorf("grok egress unavailable: %w", err)
			}
			if lease.Direct {
				// The scope fell back to the general proxy.
				lease = nil
			}
		}
		if lease != nil {
			leaseNodeID = lease.NodeID
			req.Header.Set("User-Agent", lease.UserAgent)
			applyChromiumClientHints(req.Header, lease.UserAgent)
			if lease.CFCookies != "" {
				mergeCFCookies(req.Header, lease.CFCookies)
			}
			releaseLease = lease.Release
		}

		do := httpClient.Do
		if lease != nil {
			do = lease.Do
		}
		diagnosticAttempt := debug.BeginUpstream(ctx, method, reqURL, req.Header, body)
		resp, err = doUpstreamHTTP(req, do, c.cfg.GrokWebStreamIdleTimeout(), upstreamIdleBytes)
		diagnosticAttempt.Response(resp, err)
		if err != nil {
			if c.egress != nil && leaseNodeID != "" {
				c.egress.FeedbackOutcome(leaseNodeID, egress.OutcomeTransportError)
			}
			releaseLease()
			if attempt >= maxRetries {
				return nil, err
			}
			delay := backoffDelay(baseDelay, retry429Delay, lastDelay, attempt, 0, 0)
			lastDelay = delay
			if !util.SleepWithContext(ctx, delay) {
				return nil, ctx.Err()
			}
			continue
		}
		resp.Body = diagnosticAttempt.CaptureBody(resp.Body)
		if resp.StatusCode == okStatus {
			if c.egress != nil && leaseNodeID != "" {
				c.egress.FeedbackOutcome(leaseNodeID, egress.OutcomeSuccess)
			}
			if lease != nil {
				resp.Body = &leaseResponseBody{ReadCloser: resp.Body, release: releaseLease}
			}
			return resp, nil
		}

		lastStatus = resp.StatusCode
		raw, headerCopy := readBoundedResponse(resp)
		lastBody = string(raw)

		if lastStatus == http.StatusTooManyRequests {
			// Web/app-chat must use the same scoped registry as Console and Build.
			// noteTeamRateLimit only recorded a raw TeamID key, while request
			// admission checks provider+account/team identities; that mismatch made
			// every later Web request miss the cooldown and hit upstream again.
			token, model := appChatRateLimitRequest(body, headers)
			if meta := noteScopedRateLimit(ctx, ProviderWeb, token, model, lastStatus, resp.Header, raw); meta != nil {
				if desc := meta.Describe(); desc != "" {
					lastBody = lastBody + " " + desc
				}
			}
			if c.egress != nil && leaseNodeID != "" {
				c.egress.FeedbackOutcome(leaseNodeID, egress.OutcomeRateLimited)
			}
			releaseLease()
			if retry429 && attempt < maxRetries {
				slog.Debug("doRequest retry triggered", "status", lastStatus, "attempt", attempt, "body", lastBody)
				retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
				delay := backoffDelay(baseDelay, retry429Delay, lastDelay, attempt, lastStatus, retryAfter)
				lastDelay = delay
				if !util.SleepWithContext(ctx, delay) {
					return nil, ctx.Err()
				}
				continue
			}
			return nil, newUpstreamError(lastStatus, headerCopy, []byte(lastBody), leaseNodeID)
		}

		// Response-aware classification: Cloudflare/DPoP challenges are egress
		// problems, never account problems.
		kind := ClassifyUpstreamResponse(lastStatus, resp.Header, raw)
		if kind == UpstreamErrorCloudflareChallenge {
			recordUpstreamChallenge("cloudflare")
			if lease != nil && !challengeRetried {
				// Invalidate this exact clearance and re-acquire once. The retry
				// does not consume the attempt budget (decrement before continue).
				challengeRetried = true
				if lease != nil {
					lease.InvalidateClearance()
				}
				releaseLease()
				attempt--
				continue
			}
		} else if kind == UpstreamErrorDPoPChallenge {
			recordUpstreamChallenge("dpop")
		} else if kind == UpstreamErrorGenericForbidden {
			recordGenericForbidden()
			if lease != nil {
				lease.InvalidateClearance()
			}
		}

		if c.egress != nil && leaseNodeID != "" {
			c.egress.FeedbackOutcome(leaseNodeID, egressOutcomeForKind(kind))
		}
		releaseLease()
		return nil, newUpstreamError(lastStatus, headerCopy, raw, leaseNodeID)
	}

	// Loop always returns; this trailing return satisfies the compiler.
	return nil, newUpstreamError(lastStatus, nil, []byte(lastBody), "")
}

// egressScopeForURL maps an upstream URL to an egress node scope.
func (c *Client) egressScopeForURL(reqURL string) string {
	host := strings.ToLower(strings.TrimSpace(reqURL))
	switch {
	case strings.Contains(host, "cli-chat-proxy.grok.com"):
		return "cli"
	case strings.Contains(host, "assets.grok.com"):
		return "asset"
	default:
		return "app_chat"
	}
}

// requestCredentialAffinity derives account affinity from the SSO cookie
// already attached to a request. It never exposes the credential in manager
// state or cache keys. Non-authenticated requests retain a URL-specific key.
func requestCredentialAffinity(header http.Header, reqURL string) string {
	for _, item := range grokCookieItems(header.Get("Cookie")) {
		if item.name == "sso" || item.name == "sso-rw" {
			return credentialAffinity(item.value)
		}
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(reqURL))))
	return fmt.Sprintf("url_%x", sum[:12])
}

// mergeCFCookies injects Cloudflare clearance cookies into an existing Cookie
// header without clobbering the SSO cookies already set.
func mergeCFCookies(header http.Header, cfCookies string) {
	if strings.TrimSpace(cfCookies) == "" {
		return
	}
	existing := strings.TrimSpace(header.Get("Cookie"))
	switch {
	case existing == "":
		header.Set("Cookie", cfCookies)
	default:
		header.Set("Cookie", existing+"; "+cfCookies)
	}
}
