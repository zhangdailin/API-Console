package httpclient

import (
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
)

// HttpClientCache provides a thread-safe cache for http.Client instances
// based on their proxy configuration. This ensures that we reuse TCP connections
// (Keep-Alive) instead of exhausting ephemeral ports and paying the TLS handshake
// penalty on every upstream request.
type clientPool struct {
	mu      sync.RWMutex
	clients map[string]*http.Client
}

const maxSharedHTTPClients = 512

// evictOneClientLocked bounds process-wide connection pools. Callers hold p.mu.
// A deterministic LRU is unnecessary here because keys are configuration
// identities; bounding and closing any stale pool prevents unbounded growth.
func (p *clientPool) evictOneClientLocked() {
	if p == nil || len(p.clients) < maxSharedHTTPClients {
		return
	}
	for key, client := range p.clients {
		delete(p.clients, key)
		if client != nil {
			client.CloseIdleConnections()
		}
		return
	}
}

var httpClientCache = clientPool{clients: make(map[string]*http.Client)}

// GetSharedHTTPClient returns a shared http.Client.
// The proxyKey should uniquely identify the proxy configuration (e.g., the Proxy URL or "direct").
// Transport configuration (like timeouts) should be uniform per proxyKey.
func GetSharedHTTPClient(proxyKey string, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	return GetSharedHTTPClientWithHTTP2(proxyKey, timeout, proxyFunc, false)
}

// GetSharedHTTPClientWithHTTP2 is GetSharedHTTPClient plus an explicit HTTP/2
// choice, for a caller that has to match a client the upstream is known to
// compare against.
//
// It is opt-in and off by default on purpose. With ForceAttemptHTTP2 the
// transport multiplexes every request to a host over one connection, which
// changes the failure mode of a single reset from "one request failed" to "every
// in-flight request failed", and Go queues rather than opens a new connection
// when the peer's stream limit is reached — so it is a poor default for a
// concurrency-100 gateway. The flag is part of the cache key so one caller's
// choice cannot silently become another's.
func GetSharedHTTPClientWithHTTP2(proxyKey string, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error), http2 bool) *http.Client {
	return GetSharedHTTPClientWithLimits(proxyKey, timeout, proxyFunc, http2, nil)
}

func HTTPPoolLimits(cfg *config.Config) (maxConns, maxIdle int) {
	maxConns, maxIdle = 200, 100
	if cfg != nil {
		if cfg.UpstreamMaxConnsPerHost > 0 {
			maxConns = min(cfg.UpstreamMaxConnsPerHost, 16384)
		}
		if cfg.UpstreamMaxIdleConnsPerHost > 0 {
			maxIdle = min(cfg.UpstreamMaxIdleConnsPerHost, maxConns)
		}
	}
	maxIdle = min(maxIdle, maxConns)
	return
}

func GetSharedHTTPClientWithLimits(proxyKey string, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error), http2 bool, cfg *config.Config) *http.Client {
	return GetSharedHTTPClientWithTimeouts(proxyKey, timeout, responseHeaderTimeoutForClient(timeout), proxyFunc, http2, cfg)
}

// GetSharedHTTPClientWithTimeouts isolates streaming header budgets without
// changing the transport policy of other callers sharing the same proxy.
func GetSharedHTTPClientWithTimeouts(proxyKey string, timeout, headerTimeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error), http2 bool, cfg *config.Config) *http.Client {
	if proxyKey == "" {
		proxyKey = "direct"
	}
	cacheKey := sharedHTTPClientCacheKey(proxyKey, timeout)
	cacheKey += fmt.Sprintf("|header-timeout=%d", headerTimeout)
	maxConns, maxIdle := HTTPPoolLimits(cfg)
	cacheKey += fmt.Sprintf("|connections=%d|idle=%d", maxConns, maxIdle)
	if http2 {
		cacheKey += "|h2"
	}

	httpClientCache.mu.RLock()
	client, ok := httpClientCache.clients[cacheKey]
	httpClientCache.mu.RUnlock()
	if ok {
		return client
	}

	httpClientCache.mu.Lock()
	defer httpClientCache.mu.Unlock()

	// Double check
	if client, ok = httpClientCache.clients[cacheKey]; ok {
		return client
	}

	transport := &http.Transport{
		MaxIdleConns:          maxIdle,
		MaxIdleConnsPerHost:   maxIdle,
		MaxConnsPerHost:       maxConns,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		Proxy:                 proxyFunc,
		TLSClientConfig:       &tls.Config{},
		ForceAttemptHTTP2:     http2,
	}

	newClient := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	httpClientCache.evictOneClientLocked()
	httpClientCache.clients[cacheKey] = newClient
	return newClient
}

func sharedHTTPClientCacheKey(proxyKey string, timeout time.Duration) string {
	return fmt.Sprintf("%s|timeout=%d", proxyKey, int64(timeout/time.Second))
}

func responseHeaderTimeoutForClient(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 30 * time.Second
	}
	if timeout <= 30*time.Second {
		return timeout
	}
	return min(max(timeout/2, 60*time.Second), 120*time.Second)
}

func GenerateProxyKeyFromConfig(cfg *config.Config) string {
	if cfg == nil {
		return "env"
	}
	// Never put credentials in a cache key: include only their digest. The
	// transport captures its proxy function on first use, so changing a proxy
	// password must allocate a fresh client rather than reusing stale auth.
	credentials := fmt.Sprintf("|credentials=%x", sha256.Sum256([]byte(cfg.ProxyUser+"\x00"+cfg.ProxyPass)))
	if proxyURL := strings.TrimSpace(cfg.ProxyURL); proxyURL != "" {
		// Keep passwords out of cache keys, including URL-embedded credentials.
		key := fmt.Sprintf("proxy-url:%x", sha256.Sum256([]byte(proxyURL))) + credentials
		if len(cfg.ProxyBypass) > 0 {
			key += "|" + strings.Join(cfg.ProxyBypass, ",")
		}
		return key
	}
	key := fmt.Sprintf("split-proxy:%x", sha256.Sum256([]byte(cfg.ProxyHTTP+"\x00"+cfg.ProxyHTTPS+"\x00"+cfg.ProxyUser))) + credentials
	if len(cfg.ProxyBypass) > 0 {
		key += "|" + strings.Join(cfg.ProxyBypass, ",")
	}
	return key
}
