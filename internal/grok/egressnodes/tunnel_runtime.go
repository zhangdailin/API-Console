package egressnodes

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Tunnel nodes (trojan, vless, ss, vmess share links) are served by a local
// SOCKS5 listener per link on 127.0.0.1. The HTTP and browser transports dial
// that listener like any socks5h proxy; each CONNECT is carried to the remote
// server by the link's protocol. Listeners live for the process and are shared
// by every node using the same link.

type tunnelDialer func(ctx context.Context, target string) (net.Conn, error)

type tunnel struct {
	listener net.Listener
	endpoint *url.URL
	dial     tunnelDialer
}

var tunnels struct {
	sync.Mutex
	byLink map[string]*tunnel
}

const tunnelDialTimeout = 15 * time.Second

// tunnelEndpoint returns the local socks5h endpoint of a share link, starting
// its listener on first use.
func tunnelEndpoint(raw string) (*url.URL, error) {
	info, err := parseTunnelURL(raw)
	if err != nil {
		return nil, err
	}
	key := Fingerprint(raw)
	tunnels.Lock()
	defer tunnels.Unlock()
	if tunnels.byLink == nil {
		tunnels.byLink = map[string]*tunnel{}
	}
	if t, ok := tunnels.byLink[key]; ok {
		return t.endpoint, nil
	}
	dial, err := protocolDialer(info)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("tunnel listener: %w", err)
	}
	t := &tunnel{listener: listener, dial: dial, endpoint: &url.URL{Scheme: "socks5h", Host: listener.Addr().String()}}
	tunnels.byLink[key] = t
	go t.serve()
	return t.endpoint, nil
}

// tunnelRoutable reports whether a share link can carry traffic now.
func tunnelRoutable(raw string) error {
	info, err := parseTunnelURL(raw)
	if err != nil {
		return err
	}
	_, err = protocolDialer(info)
	return err
}

func protocolDialer(info TunnelInfo) (tunnelDialer, error) {
	switch info.Protocol {
	case "trojan":
		return trojanDialer(info), nil
	case "vless":
		return vlessDialer(info), nil
	case "ss":
		return shadowsocksDialer(info)
	case "vmess":
		return vmessDialer(info)
	}
	return nil, fmt.Errorf("unsupported tunnel protocol %q", info.Protocol)
}

func (t *tunnel) serve() {
	for {
		conn, err := t.listener.Accept()
		if err != nil {
			return
		}
		go t.handle(conn)
	}
}

// handle speaks the minimal SOCKS5 the local transports use: no
// authentication, CONNECT only.
func (t *tunnel) handle(client net.Conn) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(tunnelDialTimeout))
	var head [2]byte
	if _, err := io.ReadFull(client, head[:]); err != nil || head[0] != 5 {
		return
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(client, methods); err != nil {
		return
	}
	if _, err := client.Write([]byte{5, 0}); err != nil {
		return
	}
	var req [4]byte
	if _, err := io.ReadFull(client, req[:]); err != nil || req[1] != 1 {
		_, _ = client.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	target, err := readSocksAddress(client, req[3])
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), tunnelDialTimeout)
	remote, err := t.dial(ctx, target)
	cancel()
	if err != nil {
		_, _ = client.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer remote.Close()
	if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	pipe(client, remote)
}

func readSocksAddress(r io.Reader, atyp byte) (string, error) {
	var host string
	switch atyp {
	case 1:
		var ip [4]byte
		if _, err := io.ReadFull(r, ip[:]); err != nil {
			return "", err
		}
		host = net.IP(ip[:]).String()
	case 4:
		var ip [16]byte
		if _, err := io.ReadFull(r, ip[:]); err != nil {
			return "", err
		}
		host = net.IP(ip[:]).String()
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return "", err
		}
		name := make([]byte, n[0])
		if _, err := io.ReadFull(r, name); err != nil {
			return "", err
		}
		host = string(name)
	default:
		return "", errors.New("unsupported address type")
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), nil
}

// socksAddress encodes host:port as ATYP|ADDR|PORT, the address form trojan,
// shadowsocks and (with different type codes) vless and vmess share.
func socksAddress(target string, ipv4, domain, ipv6 byte) ([]byte, error) {
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return nil, errors.New("invalid port")
	}
	var out []byte
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			out = append([]byte{ipv4}, v4...)
		} else {
			out = append([]byte{ipv6}, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, errors.New("host name too long")
		}
		out = append([]byte{domain, byte(len(host))}, host...)
	}
	return binary.BigEndian.AppendUint16(out, uint16(port)), nil
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	copyHalf := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go copyHalf(a, b)
	go copyHalf(b, a)
	<-done
	_ = a.SetDeadline(time.Now().Add(5 * time.Second))
	_ = b.SetDeadline(time.Now().Add(5 * time.Second))
	<-done
}

// dialTransport opens the carrier connection to the tunnel server: TCP, with
// TLS when the link asks for it, and a WebSocket on top for ws links.
func dialTransport(ctx context.Context, info TunnelInfo) (net.Conn, error) {
	server := net.JoinHostPort(info.Host, info.Port)
	tlsConfig := (*tls.Config)(nil)
	if info.Security == "tls" {
		sni := info.SNI
		if sni == "" {
			sni = info.WSHost
		}
		if sni == "" {
			sni = info.Host
		}
		tlsConfig = &tls.Config{ServerName: sni, InsecureSkipVerify: info.Insecure, MinVersion: tls.VersionTLS12} //nolint:gosec // opt-in per share link (allowInsecure=1)
	}
	if info.Network == "ws" {
		scheme := "ws"
		if tlsConfig != nil {
			scheme = "wss"
		}
		path := info.Path
		if path == "" {
			path = "/"
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		host := info.WSHost
		if host == "" {
			host = info.Host
		}
		dialer := websocket.Dialer{
			HandshakeTimeout: tunnelDialTimeout,
			TLSClientConfig:  tlsConfig,
			NetDialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: tunnelDialTimeout}).DialContext(ctx, network, server)
			},
		}
		header := http.Header{"Host": {host}, "User-Agent": {"Mozilla/5.0"}}
		ws, resp, err := dialer.DialContext(ctx, scheme+"://"+net.JoinHostPort(host, info.Port)+path, header)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			return nil, fmt.Errorf("tunnel websocket: %w", err)
		}
		return &wsConn{Conn: ws}, nil
	}
	conn, err := (&net.Dialer{Timeout: tunnelDialTimeout}).DialContext(ctx, "tcp", server)
	if err != nil {
		return nil, err
	}
	if tlsConfig == nil {
		return conn, nil
	}
	tlsConn := tls.Client(conn, tlsConfig)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("tunnel tls: %w", err)
	}
	return tlsConn, nil
}

// wsConn adapts a WebSocket to a byte stream of binary messages.
type wsConn struct {
	*websocket.Conn
	reader  io.Reader
	writeMu sync.Mutex
}

func (c *wsConn) Read(p []byte) (int, error) {
	for {
		if c.reader == nil {
			_, reader, err := c.NextReader()
			if err != nil {
				return 0, err
			}
			c.reader = reader
		}
		n, err := c.reader.Read(p)
		if errors.Is(err, io.EOF) {
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}
