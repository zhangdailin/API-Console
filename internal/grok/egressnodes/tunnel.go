package egressnodes

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// TunnelInfo is a parsed share link.
type TunnelInfo struct {
	Protocol string // trojan|vless|ss|vmess
	Name     string
	Host     string
	Port     string
	Secret   string // trojan password, vless/vmess UUID, ss password
	Method   string // ss cipher, vmess security
	Security string // tls|none
	SNI      string
	Network  string // tcp|ws
	Path     string
	WSHost   string
	Insecure bool
	AlterID  int
	Flow     string
}

// ssCiphers are the Shadowsocks AEAD ciphers supported.
var ssCiphers = map[string]bool{"aes-128-gcm": true, "aes-256-gcm": true, "chacha20-ietf-poly1305": true}

func decodeBase64Loose(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if out, err := encoding.DecodeString(value); err == nil {
			return out, nil
		}
	}
	return nil, errors.New("invalid base64")
}

func parseTunnelURL(raw string) (TunnelInfo, error) {
	raw = strings.TrimSpace(raw)
	scheme, _, _ := strings.Cut(raw, "://")
	switch strings.ToLower(scheme) {
	case "vmess":
		return parseVMess(raw)
	case "ss":
		return parseShadowsocks(raw)
	case "trojan", "vless":
		return parseURLTunnel(raw)
	}
	return TunnelInfo{}, fmt.Errorf("unsupported tunnel protocol %q", scheme)
}

func parseURLTunnel(raw string) (TunnelInfo, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return TunnelInfo{}, errors.New("invalid share link")
	}
	q := parsed.Query()
	info := TunnelInfo{Protocol: strings.ToLower(parsed.Scheme), Name: parsed.Fragment, Host: parsed.Hostname(), Port: parsed.Port(),
		SNI: q.Get("sni"), Network: strings.ToLower(q.Get("type")), Path: q.Get("path"), WSHost: q.Get("host"), Flow: q.Get("flow")}
	if parsed.User != nil {
		info.Secret = parsed.User.Username()
		if info.Secret == "" {
			info.Secret, _ = parsed.User.Password()
		}
	}
	info.Security = strings.ToLower(q.Get("security"))
	if info.Security == "" {
		if info.Protocol == "trojan" {
			info.Security = "tls"
		} else {
			info.Security = "none"
		}
	}
	info.Insecure = q.Get("allowInsecure") == "1" || q.Get("insecure") == "1"
	return info, info.validate()
}

func parseShadowsocks(raw string) (TunnelInfo, error) {
	body := strings.TrimPrefix(strings.TrimSpace(raw), "ss://")
	body, name, _ := strings.Cut(body, "#")
	name, _ = url.PathUnescape(name)
	body, _, _ = strings.Cut(body, "?")
	info := TunnelInfo{Protocol: "ss", Name: name, Network: "tcp", Security: "none"}
	var hostPort string
	if userinfo, server, ok := strings.Cut(body, "@"); ok {
		// SIP002: base64(method:password)@host:port, or plain method:password.
		hostPort = server
		credentials := userinfo
		if decoded, err := decodeBase64Loose(userinfo); err == nil {
			credentials = string(decoded)
		} else if unescaped, err := url.PathUnescape(userinfo); err == nil {
			credentials = unescaped
		}
		info.Method, info.Secret, _ = strings.Cut(credentials, ":")
	} else {
		decoded, err := decodeBase64Loose(body)
		if err != nil {
			return TunnelInfo{}, errors.New("invalid ss link")
		}
		credentials, server, ok := strings.Cut(string(decoded), "@")
		if !ok {
			return TunnelInfo{}, errors.New("invalid ss link")
		}
		hostPort = server
		info.Method, info.Secret, _ = strings.Cut(credentials, ":")
	}
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return TunnelInfo{}, errors.New("invalid ss server address")
	}
	info.Host, info.Port = host, port
	info.Method = strings.ToLower(info.Method)
	return info, info.validate()
}

func parseVMess(raw string) (TunnelInfo, error) {
	decoded, err := decodeBase64Loose(strings.TrimPrefix(strings.TrimSpace(raw), "vmess://"))
	if err != nil {
		return TunnelInfo{}, errors.New("invalid vmess link")
	}
	var doc map[string]any
	if err := json.Unmarshal(decoded, &doc); err != nil {
		return TunnelInfo{}, errors.New("invalid vmess link")
	}
	text := func(key string) string {
		switch v := doc[key].(type) {
		case string:
			return strings.TrimSpace(v)
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return ""
	}
	info := TunnelInfo{Protocol: "vmess", Name: text("ps"), Host: text("add"), Port: text("port"), Secret: text("id"), Method: strings.ToLower(text("scy")),
		Network: strings.ToLower(text("net")), Path: text("path"), WSHost: text("host"), SNI: text("sni")}
	info.AlterID, _ = strconv.Atoi(text("aid"))
	info.Security = "none"
	if strings.EqualFold(text("tls"), "tls") {
		info.Security = "tls"
	}
	if info.Method == "" {
		info.Method = "auto"
	}
	return info, info.validate()
}

func (t TunnelInfo) validate() error {
	if t.Host == "" || t.Port == "" {
		return fmt.Errorf("%s link missing server address", t.Protocol)
	}
	if n, err := strconv.Atoi(t.Port); err != nil || n <= 0 || n > 65535 {
		return fmt.Errorf("%s link has an invalid port", t.Protocol)
	}
	if t.Secret == "" {
		return fmt.Errorf("%s link missing credentials", t.Protocol)
	}
	if t.Network == "" {
		t.Network = "tcp"
	}
	switch t.Network {
	case "tcp", "ws", "":
	default:
		return fmt.Errorf("%s transport %q is not supported (tcp or ws only)", t.Protocol, t.Network)
	}
	switch t.Protocol {
	case "ss":
		if !ssCiphers[t.Method] {
			return fmt.Errorf("ss cipher %q is not supported (aes-128-gcm, aes-256-gcm, chacha20-ietf-poly1305)", t.Method)
		}
	case "vless":
		if t.Security != "tls" && t.Security != "none" {
			return fmt.Errorf("vless security %q is not supported (tls or none)", t.Security)
		}
		if t.Flow != "" {
			return fmt.Errorf("vless flow %q is not supported", t.Flow)
		}
	case "vmess":
		if t.AlterID != 0 {
			return errors.New("vmess alterId must be 0 (AEAD)")
		}
	}
	return nil
}

func validateTunnelURL(raw string) error {
	if strings.Contains(raw, "{account}") {
		return errors.New("tunnel links cannot use {account}")
	}
	_, err := parseTunnelURL(raw)
	return err
}

// TunnelName returns the display name a share link carries.
func TunnelName(raw string) string {
	info, err := parseTunnelURL(raw)
	if err != nil {
		return ""
	}
	return info.Name
}
