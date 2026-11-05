package egressnodes

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // VMess derives its command key with MD5
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/fnv"
	"io"
	mrand "math/rand/v2"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// VMess with AEAD headers (alterId 0), following v2ray-core: the request
// header is sealed under keys derived from the command key, the AuthID and a
// connection nonce; the body is a chunk stream whose 2-byte lengths are masked
// with SHAKE128 of the body IV, sealed with AES-128-GCM or ChaCha20-Poly1305
// under a counter nonce.

const (
	vmessOptChunkStream  = 0x01
	vmessOptChunkMasking = 0x04
	vmessSecAES128GCM    = 3
	vmessSecChaCha20     = 4
	vmessMaxChunk        = 8192
)

type kdfNode struct {
	parent *kdfNode
	value  []byte
}

func (n *kdfNode) create() hash.Hash {
	if n.parent == nil {
		return hmac.New(sha256.New, n.value)
	}
	return hmac.New(n.parent.create, n.value)
}

// vmessKDF is v2ray's nested-HMAC key derivation rooted at "VMess AEAD KDF".
func vmessKDF(key []byte, path ...[]byte) []byte {
	node := &kdfNode{value: []byte("VMess AEAD KDF")}
	for _, p := range path {
		node = &kdfNode{parent: node, value: p}
	}
	h := node.create()
	h.Write(key)
	return h.Sum(nil)
}

func vmessCmdKey(id []byte) []byte {
	h := md5.New() //nolint:gosec // protocol-defined
	h.Write(id)
	h.Write([]byte("c48619fe-8f02-49e0-b9e9-edf763e17e21"))
	return h.Sum(nil)
}

func aesGCM(key []byte) cipher.AEAD {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return aead
}

func vmessAuthID(cmdKey []byte, now time.Time) []byte {
	var plain [16]byte
	binary.BigEndian.PutUint64(plain[:8], uint64(now.Unix()))
	_, _ = rand.Read(plain[8:12])
	binary.BigEndian.PutUint32(plain[12:], crc32.ChecksumIEEE(plain[:12]))
	block, err := aes.NewCipher(vmessKDF(cmdKey, []byte("AES Auth ID Encryption"))[:16])
	if err != nil {
		panic(err)
	}
	out := make([]byte, 16)
	block.Encrypt(out, plain[:])
	return out
}

// sealVMessHeader wraps a request header: AuthID | sealed length | nonce |
// sealed header.
func sealVMessHeader(cmdKey, header []byte, now time.Time) []byte {
	authID := vmessAuthID(cmdKey, now)
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(header)))
	lengthKey := vmessKDF(cmdKey, []byte("VMess Header AEAD Key_Length"), authID, nonce)[:16]
	lengthNonce := vmessKDF(cmdKey, []byte("VMess Header AEAD Nonce_Length"), authID, nonce)[:12]
	headerKey := vmessKDF(cmdKey, []byte("VMess Header AEAD Key"), authID, nonce)[:16]
	headerNonce := vmessKDF(cmdKey, []byte("VMess Header AEAD Nonce"), authID, nonce)[:12]
	out := append([]byte(nil), authID...)
	out = aesGCM(lengthKey).Seal(out, lengthNonce, length[:], authID)
	out = append(out, nonce...)
	return aesGCM(headerKey).Seal(out, headerNonce, header, authID)
}

func chachaKey(key []byte) []byte {
	out := make([]byte, 32)
	first := md5.Sum(key) //nolint:gosec // protocol-defined
	copy(out, first[:])
	second := md5.Sum(out[:16]) //nolint:gosec // protocol-defined
	copy(out[16:], second[:])
	return out
}

func vmessBodyAEAD(security byte, key []byte) (cipher.AEAD, error) {
	if security == vmessSecChaCha20 {
		return chacha20poly1305.New(chachaKey(key))
	}
	return aesGCM(key), nil
}

type vmessChunks struct {
	aead  cipher.AEAD
	iv    []byte
	count uint16
	shake *sha3.SHAKE
}

func newVMessChunks(aead cipher.AEAD, iv []byte) *vmessChunks {
	shake := sha3.NewSHAKE128()
	shake.Write(iv)
	return &vmessChunks{aead: aead, iv: append([]byte(nil), iv...), shake: shake}
}

func (c *vmessChunks) nonce() []byte {
	nonce := append([]byte(nil), c.iv[:c.aead.NonceSize()]...)
	binary.BigEndian.PutUint16(nonce, c.count)
	c.count++
	return nonce
}

func (c *vmessChunks) mask() uint16 {
	var b [2]byte
	_, _ = c.shake.Read(b[:])
	return binary.BigEndian.Uint16(b[:])
}

func vmessDialer(info TunnelInfo) (tunnelDialer, error) {
	id, err := parseUUID(info.Secret)
	if err != nil {
		return nil, err
	}
	var security byte
	switch info.Method {
	case "", "auto", "aes-128-gcm":
		security = vmessSecAES128GCM
	case "chacha20-poly1305", "chacha20-ietf-poly1305":
		security = vmessSecChaCha20
	default:
		return nil, fmt.Errorf("vmess security %q is not supported (auto, aes-128-gcm, chacha20-poly1305)", info.Method)
	}
	cmdKey := vmessCmdKey(id)
	return func(ctx context.Context, target string) (net.Conn, error) {
		addr, err := socksAddress(target, 1, 2, 3)
		if err != nil {
			return nil, err
		}
		port, address := addr[len(addr)-2:], addr[:len(addr)-2]
		var secret [33]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return nil, err
		}
		bodyKey, bodyIV, respAuth := secret[:16], secret[16:32], secret[32]
		header := []byte{1}
		header = append(header, bodyIV...)
		header = append(header, bodyKey...)
		padding := mrand.IntN(16)
		header = append(header, respAuth, vmessOptChunkStream|vmessOptChunkMasking, byte(padding<<4)|security, 0, 1)
		header = append(header, port...)
		header = append(header, address...)
		pad := make([]byte, padding)
		_, _ = rand.Read(pad)
		header = append(header, pad...)
		checksum := fnv.New32a()
		checksum.Write(header)
		header = checksum.Sum(header)

		writeAEAD, err := vmessBodyAEAD(security, bodyKey)
		if err != nil {
			return nil, err
		}
		respKey := sha256.Sum256(bodyKey)
		respIV := sha256.Sum256(bodyIV)
		readAEAD, err := vmessBodyAEAD(security, respKey[:16])
		if err != nil {
			return nil, err
		}
		conn, err := dialTransport(ctx, info)
		if err != nil {
			return nil, err
		}
		if _, err := conn.Write(sealVMessHeader(cmdKey, header, time.Now())); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return &vmessConn{Conn: conn, writer: newVMessChunks(writeAEAD, bodyIV), reader: newVMessChunks(readAEAD, respIV[:16]),
			respKey: respKey[:16], respIV: respIV[:16], respAuth: respAuth}, nil
	}, nil
}

type vmessConn struct {
	net.Conn
	writer, reader  *vmessChunks
	respKey, respIV []byte
	respAuth        byte
	headerOnce      sync.Once
	headerErr       error
	pending         []byte
	eof             bool
	writeMu         sync.Mutex
	closedWrite     bool
}

func (c *vmessConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	written := 0
	for len(p) > 0 {
		n := min(len(p), vmessMaxChunk)
		if err := c.writeChunk(p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (c *vmessConn) writeChunk(payload []byte) error {
	size := uint16(len(payload) + c.writer.aead.Overhead())
	buf := make([]byte, 2, 2+int(size))
	binary.BigEndian.PutUint16(buf, size^c.writer.mask())
	buf = c.writer.aead.Seal(buf, c.writer.nonce(), payload, nil)
	_, err := c.Conn.Write(buf)
	return err
}

// CloseWrite sends the empty chunk that ends a VMess request stream.
func (c *vmessConn) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closedWrite {
		return nil
	}
	c.closedWrite = true
	return c.writeChunk(nil)
}

func (c *vmessConn) readHeader() error {
	lengthKey := vmessKDF(c.respKey, []byte("AEAD Resp Header Len Key"))[:16]
	lengthIV := vmessKDF(c.respIV, []byte("AEAD Resp Header Len IV"))[:12]
	sealedLength := make([]byte, 18)
	if _, err := io.ReadFull(c.Conn, sealedLength); err != nil {
		return err
	}
	length, err := aesGCM(lengthKey).Open(nil, lengthIV, sealedLength, nil)
	if err != nil {
		return errors.New("vmess: response header rejected (check the UUID)")
	}
	headerKey := vmessKDF(c.respKey, []byte("AEAD Resp Header Key"))[:16]
	headerIV := vmessKDF(c.respIV, []byte("AEAD Resp Header IV"))[:12]
	sealed := make([]byte, int(binary.BigEndian.Uint16(length))+16)
	if _, err := io.ReadFull(c.Conn, sealed); err != nil {
		return err
	}
	header, err := aesGCM(headerKey).Open(nil, headerIV, sealed, nil)
	if err != nil || len(header) < 4 || header[0] != c.respAuth {
		return errors.New("vmess: response header mismatch")
	}
	return nil
}

func (c *vmessConn) Read(p []byte) (int, error) {
	c.headerOnce.Do(func() { c.headerErr = c.readHeader() })
	if c.headerErr != nil {
		return 0, c.headerErr
	}
	for len(c.pending) == 0 {
		if c.eof {
			return 0, io.EOF
		}
		var sizeBytes [2]byte
		if _, err := io.ReadFull(c.Conn, sizeBytes[:]); err != nil {
			return 0, err
		}
		size := int(binary.BigEndian.Uint16(sizeBytes[:]) ^ c.reader.mask())
		if size < c.reader.aead.Overhead() {
			return 0, errors.New("vmess: invalid chunk size")
		}
		sealed := make([]byte, size)
		if _, err := io.ReadFull(c.Conn, sealed); err != nil {
			return 0, err
		}
		payload, err := c.reader.aead.Open(sealed[:0], c.reader.nonce(), sealed, nil)
		if err != nil {
			return 0, errors.New("vmess: chunk authentication failed")
		}
		if len(payload) == 0 {
			c.eof = true
			continue
		}
		c.pending = payload
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}
