package egressnodes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"orchids-api/internal/httpclient"
)

// SubscriptionEntry is one exit a subscription or a pasted list describes.
type SubscriptionEntry struct {
	Name     string
	ProxyURL string
}

const maxSubscriptionBytes = 4 << 20

// FetchSubscription downloads a subscription, optionally through a proxy.
func FetchSubscription(ctx context.Context, rawURL, proxyURL string) ([]SubscriptionEntry, error) {
	target, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (target.Scheme != "https" && target.Scheme != "http") || target.Host == "" {
		return nil, errors.New("subscription URL must be http(s)")
	}
	var proxyFunc func(*http.Request) (*url.URL, error)
	if strings.TrimSpace(proxyURL) != "" {
		proxy, err := ParseProxyURL(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("subscription proxy: %w", err)
		}
		proxyFunc = http.ProxyURL(proxy)
	}
	client := httpclient.GetSharedHTTPClient("egress-subscription|"+Fingerprint(proxyURL), 30*time.Second, proxyFunc)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "clash-verge/v2.0.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("subscription download failed: " + sanitizeProbeError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subscription returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSubscriptionBytes+1))
	if err != nil {
		return nil, errors.New("subscription download failed")
	}
	if len(body) > maxSubscriptionBytes {
		return nil, errors.New("subscription exceeds 4 MiB")
	}
	entries := ParseProxyList(string(body))
	if len(entries) == 0 {
		return nil, errors.New("subscription contains no supported exits")
	}
	return entries, nil
}

// ParseProxyList reads exits from a Clash YAML document, a Base64 share-link
// list, or plain lines of proxy URLs and share links. Unsupported entries are
// skipped.
func ParseProxyList(text string) []SubscriptionEntry {
	text = strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	if text == "" {
		return nil
	}
	if entries := parseClash(text); entries != nil {
		return entries
	}
	if !strings.Contains(text, "://") {
		if decoded, err := decodeBase64Loose(strings.Join(strings.Fields(text), "")); err == nil {
			text = string(decoded)
		}
	}
	var out []SubscriptionEntry
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if ValidateProxyURL(line) != nil {
			continue
		}
		name := ""
		if IsTunnelURL(line) {
			name = TunnelName(line)
		}
		out = append(out, SubscriptionEntry{Name: name, ProxyURL: line})
	}
	return out
}

func parseClash(text string) []SubscriptionEntry {
	if !strings.Contains(text, "proxies:") {
		return nil
	}
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil || len(doc.Proxies) == 0 {
		return nil
	}
	out := []SubscriptionEntry{}
	for _, proxy := range doc.Proxies {
		if link := clashProxyURL(proxy); link != "" && ValidateProxyURL(link) == nil {
			out = append(out, SubscriptionEntry{Name: clashString(proxy, "name"), ProxyURL: link})
		}
	}
	return out
}

func clashString(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case int:
		return strconv.Itoa(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func clashBool(m map[string]any, key string) bool { return clashString(m, key) == "true" }

// clashProxyURL converts one Clash proxy to a proxy URL or share link.
func clashProxyURL(p map[string]any) string {
	server, port := clashString(p, "server"), clashString(p, "port")
	if server == "" || port == "" {
		return ""
	}
	hostPort := net.JoinHostPort(server, port)
	name := url.PathEscape(clashString(p, "name"))
	ws := map[string]any{}
	if opts, ok := p["ws-opts"].(map[string]any); ok {
		ws = opts
	}
	wsHost := ""
	if headers, ok := ws["headers"].(map[string]any); ok {
		wsHost = clashString(headers, "Host")
	}
	query := url.Values{}
	if clashString(p, "network") == "ws" {
		query.Set("type", "ws")
		if path := clashString(ws, "path"); path != "" {
			query.Set("path", path)
		}
		if wsHost != "" {
			query.Set("host", wsHost)
		}
	}
	if sni := clashString(p, "sni"); sni != "" {
		query.Set("sni", sni)
	} else if sni := clashString(p, "servername"); sni != "" {
		query.Set("sni", sni)
	}
	if clashBool(p, "skip-cert-verify") {
		query.Set("allowInsecure", "1")
	}
	switch clashString(p, "type") {
	case "http", "socks5":
		userinfo := ""
		if user := clashString(p, "username"); user != "" {
			userinfo = url.UserPassword(user, clashString(p, "password")).String() + "@"
		}
		return clashString(p, "type") + "://" + userinfo + hostPort
	case "ss":
		credentials := base64.RawURLEncoding.EncodeToString([]byte(clashString(p, "cipher") + ":" + clashString(p, "password")))
		return "ss://" + credentials + "@" + hostPort + "#" + name
	case "trojan":
		query.Set("security", "tls")
		return "trojan://" + url.PathEscape(clashString(p, "password")) + "@" + hostPort + "?" + query.Encode() + "#" + name
	case "vless":
		if clashBool(p, "tls") {
			query.Set("security", "tls")
		} else {
			query.Set("security", "none")
		}
		if flow := clashString(p, "flow"); flow != "" {
			query.Set("flow", flow)
		}
		return "vless://" + clashString(p, "uuid") + "@" + hostPort + "?" + query.Encode() + "#" + name
	case "vmess":
		doc := map[string]string{"v": "2", "ps": clashString(p, "name"), "add": server, "port": port, "id": clashString(p, "uuid"),
			"aid": clashString(p, "alterId"), "scy": clashString(p, "cipher"), "net": clashString(p, "network"), "path": clashString(ws, "path"), "host": wsHost, "sni": query.Get("sni")}
		if doc["aid"] == "" {
			doc["aid"] = "0"
		}
		if clashBool(p, "tls") {
			doc["tls"] = "tls"
		}
		raw, _ := json.Marshal(doc)
		return "vmess://" + base64.StdEncoding.EncodeToString(raw)
	}
	return ""
}
