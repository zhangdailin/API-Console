package egressnodes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/httpclient"
	"orchids-api/internal/store"
)

const probeTimeout = 15 * time.Second

// probeTargets returns the IPv4 and IPv6 exit-IP endpoints of a provider.
func probeTargets(provider string) (v4, v6 string) {
	if provider == "ipinfo" {
		return "https://ipinfo.io/ip", "https://v6.ipinfo.io/ip"
	}
	return "https://1.1.1.1/cdn-cgi/trace", "https://[2606:4700:4700::1111]/cdn-cgi/trace"
}

type probeLeg struct {
	ip      string
	latency time.Duration
	err     error
}

// ProbeNode tests a stored node through its own exit: the exit IP seen over
// IPv4 and IPv6, and the IPv4 round trip. It never routes a direct-less node
// through the general proxy: a node without a proxy is tested direct.
func ProbeNode(ctx context.Context, node *store.GrokEgressNode, proxyURL, provider string) *store.GrokEgressProbe {
	result := &store.GrokEgressProbe{NodeID: node.ID, ProbedAt: time.Now(), Provider: provider, Status: "unhealthy"}
	proxy, err := Node{ProxyURL: proxyURL}.ProxyFor("probe")
	if err != nil {
		result.Error = err.Error()
		return result
	}
	var proxyFunc func(*http.Request) (*url.URL, error)
	proxyKey := ""
	if proxy != nil {
		proxyFunc = http.ProxyURL(proxy)
		proxyKey = proxy.String()
	}
	client := httpclient.GetSharedHTTPClient("egress-test:"+node.ID+"|"+Fingerprint(proxyKey), probeTimeout, proxyFunc)
	v4URL, v6URL := probeTargets(provider)
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var wg sync.WaitGroup
	legs := make([]probeLeg, 2)
	for i, target := range []string{v4URL, v6URL} {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			legs[i] = probeOnce(ctx, client, target, provider)
		}(i, target)
	}
	wg.Wait()
	result.IPv4, result.IPv6 = legs[0].ip, legs[1].ip
	switch {
	case legs[0].err == nil:
		result.Status, result.ExitIP, result.LatencyMS = "healthy", legs[0].ip, legs[0].latency.Milliseconds()
	case legs[1].err == nil:
		result.Status, result.ExitIP, result.LatencyMS = "healthy", legs[1].ip, legs[1].latency.Milliseconds()
	default:
		result.Error = sanitizeProbeError(legs[0].err)
	}
	return result
}

func probeOnce(ctx context.Context, client *http.Client, target, provider string) probeLeg {
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return probeLeg{err: err}
	}
	req.Header.Set("User-Agent", "curl/8.5.0")
	resp, err := client.Do(req)
	if err != nil {
		return probeLeg{err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return probeLeg{err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return probeLeg{err: fmt.Errorf("probe status %d", resp.StatusCode)}
	}
	ip := parseProbeIP(string(body), provider)
	if ip == "" {
		return probeLeg{err: errors.New("probe returned no IP")}
	}
	return probeLeg{ip: ip, latency: time.Since(started)}
}

func parseProbeIP(body, provider string) string {
	if provider == "ipinfo" {
		return strings.TrimSpace(body)
	}
	for _, line := range strings.Split(body, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "ip="); ok {
			return value
		}
	}
	return ""
}

// sanitizeProbeError strips anything that could echo proxy credentials: Go's
// proxy errors quote the proxy URL, userinfo included.
func sanitizeProbeError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, marker := range []string{"proxyconnect", "socks connect", "context deadline exceeded", "connection refused", "no such host", "i/o timeout", "EOF", "probe status", "probe returned"} {
		if strings.Contains(message, marker) {
			switch marker {
			case "context deadline exceeded", "i/o timeout":
				return "timeout"
			case "proxyconnect", "socks connect":
				return "proxy connection failed"
			case "probe status", "probe returned":
				return message
			default:
				return marker
			}
		}
	}
	return "connection failed"
}
