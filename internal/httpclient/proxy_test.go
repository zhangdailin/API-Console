package httpclient

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

func TestProxyFunc_NoSchemeDefaultsToHTTP(t *testing.T) {
	proxyFunc := ProxyFunc("proxy.local:3128", "", "", "", nil)
	proxyURL, err := proxyFunc(&http.Request{URL: &url.URL{Scheme: "http", Host: "example.com"}})
	testutil.NoError(t, err, "proxy func failed: %v")
	testutil.False(t, proxyURL == nil, "expected proxy url")
	testutil.Equal(t, proxyURL.Scheme, "http")
	testutil.Equal(t, proxyURL.Host, "proxy.local:3128")
}

func TestProxyFunc_WSSUsesHTTPSProxy(t *testing.T) {
	proxyFunc := ProxyFunc("http://proxy.local:3128", "http://secure.proxy:8443", "", "", nil)
	proxyURL, err := proxyFunc(&http.Request{URL: &url.URL{Scheme: "wss", Host: "example.com"}})
	testutil.NoError(t, err, "proxy func failed: %v")
	testutil.Falsef(t, proxyURL == nil || proxyURL.Host != "secure.proxy:8443", "unexpected proxy url: %v", proxyURL)
}

func TestProxyFunc_LeadingDotBypass(t *testing.T) {
	proxyFunc := ProxyFunc("http://proxy.local:3128", "", "", "", []string{".example.com"})
	proxyURL, err := proxyFunc(&http.Request{URL: &url.URL{Scheme: "https", Host: "api.example.com"}})
	testutil.NoError(t, err, "proxy func failed: %v")
	testutil.Falsef(t, proxyURL != nil, "expected bypass, got %v", proxyURL)
}

func TestProxyFuncFromConfig_ProxyURL(t *testing.T) {
	proxyFunc := ProxyFuncFromConfig(&config.Config{
		ProxyURL:    "http://user:pass@proxy.local:3128/",
		ProxyBypass: []string{"internal.local"},
	})

	proxyURL, err := proxyFunc(&http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}})
	testutil.NoError(t, err, "proxy func failed: %v")
	testutil.Falsef(t, proxyURL == nil || proxyURL.Host != "proxy.local:3128", "unexpected proxy url: %v", proxyURL)
	testutil.Falsef(t, proxyURL.User == nil || proxyURL.User.Username() != "user", "unexpected proxy user: %v", proxyURL.User)
	if pass, ok := proxyURL.User.Password(); !ok || pass != "pass" {
		t.Fatalf("unexpected proxy password")
	}
}

func TestProxyFuncFromConfig_EmptyMeansDirect(t *testing.T) {
	proxyFunc := ProxyFuncFromConfig(&config.Config{})
	proxyURL, err := proxyFunc(&http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}})
	testutil.NoError(t, err, "proxy func failed: %v")
	testutil.Falsef(t, proxyURL != nil, "expected direct connection, got %v", proxyURL)
}

func TestProxyFuncFromConfig_SeparateCredentialsApplyToURL(t *testing.T) {
	cfg := &config.Config{ProxyURL: "http://proxy.local:3128", ProxyUser: "alice", ProxyPass: "first"}
	request := &http.Request{URL: &url.URL{Scheme: "https", Host: "upstream.example"}}
	first, err := ProxyFuncFromConfig(cfg)(request)
	testutil.Falsef(t, err != nil || first == nil || first.User == nil, "first proxy = %v, error = %v", first, err)
	pass, _ := first.User.Password()
	testutil.Falsef(t, pass != "first", "first password = %q", pass)
	firstKey := GenerateProxyKeyFromConfig(cfg)
	cfg.ProxyPass = "rotated"
	secondKey := GenerateProxyKeyFromConfig(cfg)
	testutil.False(t, firstKey == secondKey || strings.Contains(firstKey, "first") || strings.Contains(secondKey, "rotated"), "proxy cache did not isolate or redact credentials")
	second, err := ProxyFuncFromConfig(cfg)(request)
	testutil.Falsef(t, err != nil || second == nil || second.User == nil, "second proxy = %v, error = %v", second, err)
	pass, _ = second.User.Password()
	testutil.Falsef(t, pass != "rotated", "rotated password = %q", pass)
}

func TestProxyCacheKeyDoesNotExposeURLCredentials(t *testing.T) {
	key := GenerateProxyKeyFromConfig(&config.Config{ProxyURL: "http://alice:very-secret@proxy.local:3128"})
	testutil.MustNotContainAny(t, key, "very-secret", "alice")
}

func TestParseProxyURL_Socks5(t *testing.T) {
	proxyURL, err := ParseProxyURL("socks5://user:pass@127.0.0.1:1080/")
	testutil.NoError(t, err, "ParseProxyURL() error = %v")
	testutil.False(t, proxyURL == nil, "expected proxy url")
	testutil.Equal(t, proxyURL.Scheme, "socks5")
	testutil.Equal(t, proxyURL.Host, "127.0.0.1:1080")
}

func TestParseProxyURLRejectsMalformedValues(t *testing.T) {
	for _, raw := range []string{"ftp://proxy.local:3128", "http://", "not a proxy"} {
		_, err := ParseProxyURL(raw)
		if err == nil {
			t.Fatalf("ParseProxyURL(%q) should fail", raw)
		}
	}
}

func TestProxyFuncFromConfigFailsClosedOnMalformedProxy(t *testing.T) {
	proxyFunc := ProxyFuncFromConfig(&config.Config{ProxyURL: "ftp://proxy.local:3128"})
	proxyURL, err := proxyFunc(&http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}})
	testutil.Error(t, err)
	testutil.True(t, proxyURL == nil)
}
