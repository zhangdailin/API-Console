package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// FlareSolverr integration: solve Cloudflare challenges for a target URL and
// return the resulting cf_clearance/__cf_bm cookies bound to a User-Agent.

const (
	maxFlareSolverrResponseBytes = 2 << 20
	maxCloudflareCookieBytes     = 4096
)

var (
	proxyCredentialPattern  = regexp.MustCompile(`(?i)\b(https?|socks4a?|socks5h?)://[^\s/@:]+:[^\s/@]+@`)
	bearerCredentialPattern = regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]+`)
	namedCredentialPattern  = regexp.MustCompile(`(?i)\b(token|password|passwd|authorization|cookie)\s*[:=]\s*[^\s,;]+`)
)

// ClearanceConfig configures how Cloudflare clearance is solved.
type ClearanceConfig struct {
	Mode            string        // "manual"|"flaresolverr"
	FlareSolverrURL string        // base URL of a FlareSolverr instance
	TargetURL       string        // e.g. "https://grok.com"
	Timeout         time.Duration // solve timeout
}

// clearanceSolution is the outcome of a Cloudflare solve.
type clearanceSolution struct {
	Cookies   string
	UserAgent string
}

type clearanceSolver interface {
	Solve(context.Context, ClearanceConfig, string) (clearanceSolution, error)
}

type flaresolverrSolver struct{}

func (flaresolverrSolver) Solve(ctx context.Context, cfg ClearanceConfig, proxyURL string) (clearanceSolution, error) {
	endpoint, err := flaresolverrEndpoint(cfg.FlareSolverrURL)
	if err != nil {
		return clearanceSolution{}, err
	}
	target := strings.TrimSpace(cfg.TargetURL)
	if target == "" {
		target = "https://grok.com"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	payload := map[string]any{
		"cmd":        "request.get",
		"url":        target,
		"maxTimeout": timeout.Milliseconds(),
	}
	if proxyURL != "" {
		payload["proxy"] = map[string]string{"url": proxyURL}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return clearanceSolution{}, fmt.Errorf("encode flaresolverr request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return clearanceSolution{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout: timeout + 15*time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("flaresolverr response must not redirect")
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return clearanceSolution{}, fmt.Errorf("call flaresolverr: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxFlareSolverrResponseBytes+1))
	if err != nil {
		return clearanceSolution{}, err
	}
	if len(responseBody) > maxFlareSolverrResponseBytes {
		return clearanceSolution{}, errors.New("flaresolverr response too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return clearanceSolution{}, fmt.Errorf("flaresolverr returned HTTP %d", response.StatusCode)
	}
	var result struct {
		Status   string `json:"status"`
		Message  string `json:"message"`
		Solution struct {
			UserAgent string `json:"userAgent"`
			Cookies   []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"cookies"`
		} `json:"solution"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return clearanceSolution{}, fmt.Errorf("parse flaresolverr response: %w", err)
	}
	if result.Status != "ok" {
		return clearanceSolution{}, fmt.Errorf("flaresolverr solve failed: %s", sanitizeFlareSolverrMessage(result.Message))
	}
	parts := make([]string, 0, len(result.Solution.Cookies))
	for _, cookie := range result.Solution.Cookies {
		if strings.TrimSpace(cookie.Name) != "" && strings.TrimSpace(cookie.Value) != "" {
			parts = append(parts, cookie.Name+"="+cookie.Value)
		}
	}
	cookies := SanitizeCloudflareCookies(strings.Join(parts, "; "))
	userAgent := strings.TrimSpace(result.Solution.UserAgent)
	if userAgent == "" || len(userAgent) > 512 || strings.IndexFunc(userAgent, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return clearanceSolution{}, errors.New("flaresolverr returned invalid User-Agent")
	}
	return clearanceSolution{Cookies: cookies, UserAgent: userAgent}, nil
}

// SanitizeCloudflareCookies keeps the complete Cloudflare challenge cookie
// family while rejecting malformed, duplicate, oversized, and control-bearing
// values. Names are normalized so callers cannot smuggle cookie attributes.
func SanitizeCloudflareCookies(raw string) string {
	parts := strings.Split(raw, ";")
	kept := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		allowed := name == "cf_clearance" || name == "__cf_bm" || name == "_cfuvid" || strings.HasPrefix(name, "cf_chl_")
		if !allowed || value == "" || len(value) > maxCloudflareCookieBytes {
			continue
		}
		if strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		kept = append(kept, name+"="+value)
	}
	return strings.Join(kept, "; ")
}

func sanitizeFlareSolverrMessage(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(value))
	value = proxyCredentialPattern.ReplaceAllString(value, "***:***@")
	value = bearerCredentialPattern.ReplaceAllString(value, "Bearer [redacted]")
	value = namedCredentialPattern.ReplaceAllString(value, "$1=[redacted]")
	if len(value) > 300 {
		value = value[:300]
	}
	return value
}

func flaresolverrEndpoint(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("flaresolverr URL invalid")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("flaresolverr URL must use http or https")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("flaresolverr URL cannot contain query or fragment")
	}
	path := strings.TrimSuffix(parsed.EscapedPath(), "/")
	if path == "" {
		path = "/v1"
	} else if path != "/v1" {
		path += "/v1"
	}
	parsed.RawPath = ""
	parsed.Path = path
	return parsed.String(), nil
}
