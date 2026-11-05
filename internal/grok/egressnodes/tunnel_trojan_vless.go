package egressnodes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
)

// Trojan: TLS to the server, then hex(SHA-224(password)) CRLF, CONNECT with a
// SOCKS-style address, CRLF, and the raw stream. The server sends no reply.
func trojanDialer(info TunnelInfo) tunnelDialer {
	sum := sha256.Sum224([]byte(info.Secret))
	auth := hex.EncodeToString(sum[:])
	return func(ctx context.Context, target string) (net.Conn, error) {
		addr, err := socksAddress(target, 1, 3, 4)
		if err != nil {
			return nil, err
		}
		conn, err := dialTransport(ctx, info)
		if err != nil {
			return nil, err
		}
		header := make([]byte, 0, len(auth)+len(addr)+5)
		header = append(header, auth...)
		header = append(header, '\r', '\n', 1)
		header = append(header, addr...)
		header = append(header, '\r', '\n')
		if _, err := conn.Write(header); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

func parseUUID(text string) ([]byte, error) {
	raw, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(text), "-", ""))
	if err != nil || len(raw) != 16 {
		return nil, errors.New("invalid UUID")
	}
	return raw, nil
}

// VLESS: version 0, UUID, no addons, CONNECT, port, address (1 IPv4,
// 2 domain, 3 IPv6). The reply starts with version and an addons block the
// client discards before the stream.
func vlessDialer(info TunnelInfo) tunnelDialer {
	return func(ctx context.Context, target string) (net.Conn, error) {
		id, err := parseUUID(info.Secret)
		if err != nil {
			return nil, err
		}
		addr, err := socksAddress(target, 1, 2, 3)
		if err != nil {
			return nil, err
		}
		// socksAddress puts the port last; VLESS wants it before the address.
		port, address := addr[len(addr)-2:], addr[:len(addr)-2]
		conn, err := dialTransport(ctx, info)
		if err != nil {
			return nil, err
		}
		header := make([]byte, 0, 22+len(address))
		header = append(header, 0)
		header = append(header, id...)
		header = append(header, 0, 1)
		header = append(header, port...)
		header = append(header, address...)
		if _, err := conn.Write(header); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return &vlessConn{Conn: conn}, nil
	}
}

type vlessConn struct {
	net.Conn
	once sync.Once
	err  error
}

func (c *vlessConn) Read(p []byte) (int, error) {
	c.once.Do(func() {
		var head [2]byte
		if _, err := io.ReadFull(c.Conn, head[:]); err != nil {
			c.err = err
			return
		}
		if head[0] != 0 {
			c.err = errors.New("vless: unexpected response version")
			return
		}
		if head[1] > 0 {
			_, c.err = io.CopyN(io.Discard, c.Conn, int64(head[1]))
		}
	})
	if c.err != nil {
		return 0, c.err
	}
	return c.Conn.Read(p)
}

func (c *vlessConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
