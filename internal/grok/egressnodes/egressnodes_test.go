package egressnodes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func newTestRegistry(t *testing.T, cfg *config.Config) (*Registry, *store.Store) {
	t.Helper()
	mini := miniredis.RunT(t)
	st, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "egress-test:", CredentialEncryptionKey: bytes.Repeat([]byte{7}, 32)})
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { current.Store(nil) })
	return Init(ctx, st, cfg), st
}

func call(t *testing.T, r *Registry, method, path string, body any) (int, string) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		testutil.NoError(t, err)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, "/api/grok/admin/egress/"+path, reader)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestNodeCredentialsAreWriteOnly(t *testing.T) {
	r, st := newTestRegistry(t, nil)
	code, body := call(t, r, http.MethodPost, "nodes", map[string]any{"name": "tokyo", "scope": "grok_web", "proxy_url": "socks5h://alice:s3cret@10.0.0.1:1080", "cloudflare_cookies": "bare-clearance-value"})
	testutil.Equal(t, code, http.StatusCreated)
	testutil.CheckNotContain(t, body, "s3cret")
	testutil.CheckNotContain(t, body, "bare-clearance-value")
	testutil.MustContain(t, body, "socks5h://***@10.0.0.1:1080")
	var created NodeDTO
	testutil.NoError(t, json.Unmarshal([]byte(body), &created))
	testutil.True(t, created.CookieConfigured)
	testutil.True(t, created.Routable)

	// Stored encrypted, decrypted for routing, cookie normalized.
	nodes, err := st.ListGrokEgressNodes(context.Background())
	testutil.NoError(t, err)
	testutil.Equal(t, nodes[0].CloudflareCookies, "cf_clearance=bare-clearance-value")
	raw, err := st.RedisClient().HGet(context.Background(), "egress-test:grok:egress:nodes", created.ID).Result()
	testutil.NoError(t, err)
	testutil.CheckNotContain(t, raw, "s3cret")

	// An update without proxy_url keeps the stored credential.
	code, _ = call(t, r, http.MethodPut, "nodes/"+created.ID, map[string]any{"name": "tokyo-2"})
	testutil.Equal(t, code, http.StatusOK)
	node, ok := Current().Node(created.ID)
	testutil.True(t, ok)
	testutil.Equal(t, node.ProxyURL, "socks5h://alice:s3cret@10.0.0.1:1080")
	testutil.Equal(t, node.Name, "tokyo-2")

	code, body = call(t, r, http.MethodGet, "nodes", nil)
	testutil.Equal(t, code, http.StatusOK)
	testutil.CheckNotContain(t, body, "s3cret")
}

func TestNodeValidationRejectsBadInput(t *testing.T) {
	r, _ := newTestRegistry(t, nil)
	for _, input := range []map[string]any{
		{"name": "", "scope": "grok_web"},
		{"name": "x", "scope": "console"},
		{"name": "x", "scope": "grok_web", "proxy_url": "ftp://host:21"},
		{"name": "x", "scope": "grok_web", "proxy_url": "http://{account}-{account}@h:1"},
		{"name": "x", "scope": "grok_web", "account_capacity": -1},
		{"name": "x", "scope": "grok_web", "proxy_profile_id": "missing"},
	} {
		code, _ := call(t, r, http.MethodPost, "nodes", input)
		testutil.Equal(t, code, http.StatusBadRequest)
	}
}

func TestImportReadsClashBase64AndPlainLists(t *testing.T) {
	clash := "proxies:\n  - {name: hk, type: ss, server: 1.2.3.4, port: 8388, cipher: aes-256-gcm, password: pw}\n  - name: us\n    type: http\n    server: 5.6.7.8\n    port: 3128\n    username: u\n    password: p\n  - {name: bad, type: hysteria2, server: 9.9.9.9, port: 1}\n"
	entries := ParseProxyList(clash)
	testutil.Equal(t, len(entries), 2)
	testutil.Equal(t, entries[0].Name, "hk")
	testutil.True(t, strings.HasPrefix(entries[0].ProxyURL, "ss://"))
	testutil.Equal(t, entries[1].ProxyURL, "http://u:p@5.6.7.8:3128")

	plain := "http://10.0.0.1:8080\n# comment\nsocks5://10.0.0.2:1080\nnot a proxy\n"
	testutil.Equal(t, len(ParseProxyList(plain)), 2)
	encoded := base64.StdEncoding.EncodeToString([]byte("trojan://pw@example.com:443?sni=example.com#jp\n"))
	links := ParseProxyList(encoded)
	testutil.Equal(t, len(links), 1)
	testutil.Equal(t, links[0].Name, "jp")

	r, _ := newTestRegistry(t, nil)
	code, body := call(t, r, http.MethodPost, "imports", map[string]any{"name": "batch", "scope": "grok_build", "content": plain})
	testutil.Equal(t, code, http.StatusCreated)
	testutil.MustContain(t, body, `"created":2`)
	testutil.Equal(t, len(Current().Candidates(store.GrokEgressScopeBuild)), 2)
}

func TestTunnelLinksRouteThroughALocalEndpoint(t *testing.T) {
	r, _ := newTestRegistry(t, nil)
	code, body := call(t, r, http.MethodPost, "nodes", map[string]any{"name": "vless", "scope": "grok_web", "proxy_url": "vless://11111111-1111-1111-1111-111111111111@example.com:443?security=tls&type=ws&path=%2Fws"})
	testutil.Equal(t, code, http.StatusCreated)
	testutil.MustContain(t, body, `"routable":true`)
	testutil.MustContain(t, body, "vless://example.com:443")
	nodes := Current().Candidates(store.GrokEgressScopeWeb)
	testutil.Equal(t, len(nodes), 1)
	endpoint, err := nodes[0].ProxyFor("sso_x")
	testutil.NoError(t, err)
	testutil.Equal(t, endpoint.Scheme, "socks5h")
	testutil.True(t, strings.HasPrefix(endpoint.Host, "127.0.0.1:"), "a tunnel is served on loopback")
	code, _ = call(t, r, http.MethodPost, "nodes", map[string]any{"name": "reality", "scope": "grok_web", "proxy_url": "vless://11111111-1111-1111-1111-111111111111@example.com:443?security=reality"})
	testutil.Equal(t, code, http.StatusBadRequest)
}

func TestFallbackModesDecideRouting(t *testing.T) {
	r, _ := newTestRegistry(t, nil)
	snap := Current()
	testutil.False(t, snap.Routes(store.GrokEgressScopeWeb), "no nodes and a direct fallback keeps the general proxy")
	ops := store.DefaultGrokEgressOperations()
	ops.Fallbacks[store.GrokEgressScopeWeb] = store.GrokEgressFallback{Mode: "none"}
	code, _ := call(t, r, http.MethodPut, "operations", ops)
	testutil.Equal(t, code, http.StatusOK)
	testutil.True(t, Current().Routes(store.GrokEgressScopeWeb), "a none fallback routes through the manager to fail closed")
	ops.Fallbacks[store.GrokEgressScopeWeb] = store.GrokEgressFallback{Mode: "fixed", NodeID: "missing"}
	code, _ = call(t, r, http.MethodPut, "operations", ops)
	testutil.Equal(t, code, http.StatusBadRequest)
}

func TestRebalanceRespectsCapacityAndBalances(t *testing.T) {
	r, st := newTestRegistry(t, nil)
	ctx := context.Background()
	for _, name := range []string{"a", "b"} {
		code, _ := call(t, r, http.MethodPost, "nodes", map[string]any{"name": name, "scope": "grok_web", "proxy_url": "http://10.0.0.1:8080", "account_capacity": 3})
		testutil.Equal(t, code, http.StatusCreated)
	}
	var refs []AccountRef
	for i := range 7 {
		refs = append(refs, AccountRef{Scope: store.GrokEgressScopeWeb, Affinity: "sso_" + string(rune('a'+i)), AccountID: int64(i + 1)})
	}
	SetAccountLister(func(context.Context) ([]AccountRef, error) { return refs, nil })
	t.Cleanup(func() { SetAccountLister(nil) })
	result, err := r.Rebalance(ctx, true)
	testutil.NoError(t, err)
	testutil.Equal(t, result.Assigned, 6)
	testutil.Equal(t, result.Unplaced, 1)
	bindings, err := st.ListGrokEgressBindings(ctx)
	testutil.NoError(t, err)
	testutil.Equal(t, len(bindings), 6)
	counts := Current().BoundCount()
	for _, node := range Current().Nodes {
		testutil.Equal(t, counts[node.ID], 3)
	}
}

func TestConfiguredNodesMigrateOnce(t *testing.T) {
	cfg := &config.Config{GrokEgressEnabled: true, GrokEgressNodes: []config.EgressNodeConfig{{Name: "old", URL: "http://10.0.0.9:3128", Scope: "all"}}}
	_, st := newTestRegistry(t, cfg)
	nodes, err := st.ListGrokEgressNodes(context.Background())
	testutil.NoError(t, err)
	testutil.Equal(t, len(nodes), 2)
	testutil.Equal(t, len(Current().Candidates(store.GrokEgressScopeBuild)), 1)
	testutil.Equal(t, len(Current().Candidates(store.GrokEgressScopeWeb)), 1)
	testutil.NoError(t, migrateConfigNodes(context.Background(), st, cfg))
	nodes, _ = st.ListGrokEgressNodes(context.Background())
	testutil.Equal(t, len(nodes), 2)
}

func TestAccountPlaceholderPinsAPoolSession(t *testing.T) {
	node := Node{ProxyURL: "http://user-session-{account}:pw@pool.example:8000"}
	first, err := node.ProxyFor("sso_alpha")
	testutil.NoError(t, err)
	second, _ := node.ProxyFor("sso_alpha")
	other, _ := node.ProxyFor("sso_beta")
	testutil.Equal(t, first.String(), second.String())
	testutil.NotEqual(t, first.String(), other.String())
	testutil.CheckNotContain(t, first.String(), "{account}")
}
