package egressnodes

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" //nolint:gosec // EVP_BytesToKey is the Shadowsocks key derivation
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // HKDF-SHA1 is the Shadowsocks AEAD subkey derivation
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Shadowsocks AEAD (SIP004): a random salt, a per-session subkey from
// HKDF-SHA1(key, salt, "ss-subkey"), then chunks of [sealed 2-byte length]
// [sealed payload] with a little-endian counter nonce. The first payload
// carries the target address.

const ssMaxPayload = 0x3FFF

type ssCipher struct {
	keyLen int
	aead   func(key []byte) (cipher.AEAD, error)
	key    []byte
}

func newSSCipher(method, password string) (*ssCipher, error) {
	c := &ssCipher{}
	switch method {
	case "aes-128-gcm", "aes-256-gcm":
		c.keyLen = 16
		if method == "aes-256-gcm" {
			c.keyLen = 32
		}
		c.aead = func(key []byte) (cipher.AEAD, error) {
			block, err := aes.NewCipher(key)
			if err != nil {
				return nil, err
			}
			return cipher.NewGCM(block)
		}
	case "chacha20-ietf-poly1305":
		c.keyLen = 32
		c.aead = chacha20poly1305.New
	default:
		return nil, fmt.Errorf("unsupported ss cipher %q", method)
	}
	c.key = evpBytesToKey(password, c.keyLen)
	return c, nil
}

// evpBytesToKey is OpenSSL's EVP_BytesToKey with MD5 and no salt.
func evpBytesToKey(password string, keyLen int) []byte {
	var out, prev []byte
	for len(out) < keyLen {
		h := md5.New() //nolint:gosec // protocol-defined
		h.Write(prev)
		h.Write([]byte(password))
		prev = h.Sum(nil)
		out = append(out, prev...)
	}
	return out[:keyLen]
}

func (c *ssCipher) session(salt []byte) (cipher.AEAD, error) {
	subkey := make([]byte, c.keyLen)
	if _, err := io.ReadFull(hkdf.New(sha1.New, c.key, salt, []byte("ss-subkey")), subkey); err != nil {
		return nil, err
	}
	return c.aead(subkey)
}

func shadowsocksDialer(info TunnelInfo) (tunnelDialer, error) {
	c, err := newSSCipher(info.Method, info.Secret)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, target string) (net.Conn, error) {
		addr, err := socksAddress(target, 1, 3, 4)
		if err != nil {
			return nil, err
		}
		conn, err := dialTransport(ctx, info)
		if err != nil {
			return nil, err
		}
		salt := make([]byte, c.keyLen)
		if _, err := rand.Read(salt); err != nil {
			_ = conn.Close()
			return nil, err
		}
		aead, err := c.session(salt)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		ss := &ssConn{Conn: conn, cipher: c, writer: aead, writeNonce: make([]byte, aead.NonceSize())}
		if _, err := conn.Write(salt); err != nil {
			_ = conn.Close()
			return nil, err
		}
		if _, err := ss.Write(addr); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return ss, nil
	}, nil
}

type ssConn struct {
	net.Conn
	cipher     *ssCipher
	writer     cipher.AEAD
	writeNonce []byte
	reader     cipher.AEAD
	readNonce  []byte
	pending    []byte
}

func increment(nonce []byte) {
	for i := range nonce {
		nonce[i]++
		if nonce[i] != 0 {
			return
		}
	}
}

func (c *ssConn) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), ssMaxPayload)
		buf := make([]byte, 0, 2+c.writer.Overhead()+n+c.writer.Overhead())
		var length [2]byte
		binary.BigEndian.PutUint16(length[:], uint16(n))
		buf = c.writer.Seal(buf, c.writeNonce, length[:], nil)
		increment(c.writeNonce)
		buf = c.writer.Seal(buf, c.writeNonce, p[:n], nil)
		increment(c.writeNonce)
		if _, err := c.Conn.Write(buf); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (c *ssConn) Read(p []byte) (int, error) {
	if len(c.pending) == 0 {
		if err := c.readChunk(); err != nil {
			return 0, err
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *ssConn) readChunk() error {
	if c.reader == nil {
		salt := make([]byte, c.cipher.keyLen)
		if _, err := io.ReadFull(c.Conn, salt); err != nil {
			return err
		}
		aead, err := c.cipher.session(salt)
		if err != nil {
			return err
		}
		c.reader, c.readNonce = aead, make([]byte, aead.NonceSize())
	}
	overhead := c.reader.Overhead()
	sealedLength := make([]byte, 2+overhead)
	if _, err := io.ReadFull(c.Conn, sealedLength); err != nil {
		return err
	}
	length, err := c.reader.Open(sealedLength[:0], c.readNonce, sealedLength, nil)
	if err != nil {
		return errors.New("ss: chunk authentication failed")
	}
	increment(c.readNonce)
	size := int(binary.BigEndian.Uint16(length)) & ssMaxPayload
	sealed := make([]byte, size+overhead)
	if _, err := io.ReadFull(c.Conn, sealed); err != nil {
		return err
	}
	payload, err := c.reader.Open(sealed[:0], c.readNonce, sealed, nil)
	if err != nil {
		return errors.New("ss: chunk authentication failed")
	}
	increment(c.readNonce)
	c.pending = payload
	return nil
}

func (c *ssConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
