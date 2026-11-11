package web

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/grok/ratelimit"
	"orchids-api/internal/httpclient"
	"orchids-api/internal/util"
	"strconv"
	"strings"
	"time"
)

func newHTTPClient(cfg *config.Config, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	proxyKey := "direct"
	if cfg != nil {
		proxyKey = httpclient.GenerateProxyKeyFromConfig(cfg)
	}

	return httpclient.GetSharedBrowserHTTPClientWithHeaderTimeout(proxyKey, timeout, 0, proxyFunc)
}

func parseRetryAfter(raw string) time.Duration {
	return ratelimit.ParseRetryAfter(raw, time.Now())
}

func parseUpstreamStatus(err error) int {
	if err == nil {
		return 0
	}
	raw := err.Error()
	_, after, ok := strings.Cut(raw, "status=")
	if !ok {
		return 0
	}
	rest := after
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
		upper := max(lastDelay*3, baseDelay)
		delay := baseDelay + time.Duration(rand.Int63n(int64(upper-baseDelay)+1))
		if delay > maxDelay {
			return maxDelay
		}
		return delay
	}

	if baseDelay <= 0 {
		baseDelay = 500 * time.Millisecond
	}
	exp := min(baseDelay*time.Duration(1<<max(0, attempt)), maxDelay)
	if exp <= 0 {
		return baseDelay
	}
	return time.Duration(rand.Int63n(int64(exp) + 1))
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
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		model = firstNonEmpty(chatwire.ParseLooseStringAny(payload["modelName"]), chatwire.ParseLooseStringAny(payload["model"]))
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

		diagnosticAttempt := debug.BeginUpstream(ctx, method, reqURL, req.Header, body)
		resp, err := doUpstreamHTTP(req, httpClient.Do, c.cfg.GrokWebStreamIdleTimeout())
		diagnosticAttempt.Response(resp, err)
		if err != nil {
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
			return resp, nil
		}

		lastStatus = resp.StatusCode
		raw, headerCopy := grokbase.ReadBoundedResponse(resp)
		lastBody = string(raw)

		if lastStatus == http.StatusTooManyRequests {
			// Web/app-chat must use the same scoped registry as Build.
			// noteTeamRateLimit only recorded a raw TeamID key, while request
			// admission checks provider+account/team identities; that mismatch made
			// every later Web request miss the cooldown and hit upstream again.
			token, model := appChatRateLimitRequest(body, headers)
			if meta := noteScopedRateLimit(ctx, ProviderWeb, token, model, lastStatus, resp.Header, raw); meta != nil {
				if desc := meta.Describe(); desc != "" {
					lastBody = lastBody + " " + desc
				}
			}
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
			return nil, newUpstreamError(lastStatus, headerCopy, []byte(lastBody))
		}

		// Response-aware classification: Cloudflare/DPoP challenges are egress
		// problems, never account problems.
		switch ClassifyUpstreamResponse(lastStatus, resp.Header, raw) {
		case UpstreamErrorCloudflareChallenge:
			recordUpstreamChallenge("cloudflare")
		case UpstreamErrorDPoPChallenge:
			recordUpstreamChallenge("dpop")
		case UpstreamErrorGenericForbidden:
			recordGenericForbidden()
		}
		return nil, newUpstreamError(lastStatus, headerCopy, raw)
	}

	// Loop always returns; this trailing return satisfies the compiler.
	return nil, newUpstreamError(lastStatus, nil, []byte(lastBody))
}
