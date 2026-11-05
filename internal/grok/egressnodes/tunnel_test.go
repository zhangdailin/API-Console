package egressnodes

import (
	"bytes"
	"crypto/aes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"hash/fnv"
	"io"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"

	"orchids-api/internal/testutil"
)

// The servers below implement each protocol's server side from its
// specification, so the client is exercised against an independent decoder:
// a round trip through SOCKS5 -> tunnel -> server -> echo target proves both
// directions, including multi-chunk payloads.

const tunnelTestUUID = "b831381d-6324-4d53-ad4f-8cda48b30811"

func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	return ln.Addr().String()
}

// serve accepts connections and hands each to handle.
func serve(t *testing.T, ln net.Listener, handle func(net.Conn)) string {
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); handle(conn) }()
		}
	}()
	return ln.Addr().String()
}

func readTarget(t *testing.T, r io.Reader, ipv4, domain, ipv6 byte, portFirst bool) string {
	var port [2]byte
	if portFirst {
		_, _ = io.ReadFull(r, port[:])
	}
	var atyp [1]byte
	_, _ = io.ReadFull(r, atyp[:])
	var host string
	switch atyp[0] {
	case ipv4:
		b := make([]byte, 4)
		_, _ = io.ReadFull(r, b)
		host = net.IP(b).String()
	case ipv6:
		b := make([]byte, 16)
		_, _ = io.ReadFull(r, b)
		host = net.IP(b).String()
	case domain:
		var n [1]byte
		_, _ = io.ReadFull(r, n[:])
		b := make([]byte, n[0])
		_, _ = io.ReadFull(r, b)
		host = string(b)
	}
	if !portFirst {
		_, _ = io.ReadFull(r, port[:])
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:]))))
}

// relay connects the decoded stream to the target and copies both ways.
func relay(target string, in io.Reader, out io.Writer) {
	remote, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	defer remote.Close()
	go func() { _, _ = io.Copy(remote, in) }()
	_, _ = io.Copy(out, remote)
}

func roundTrip(t *testing.T, link, target string) {
	t.Helper()
	testutil.NoError(t, ValidateProxyURL(link))
	endpoint, err := ParseProxyURL(link)
	testutil.NoError(t, err)
	dialer, err := proxy.SOCKS5("tcp", endpoint.Host, nil, proxy.Direct)
	testutil.NoError(t, err)
	conn, err := dialer.Dial("tcp", target)
	testutil.NoError(t, err)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	payload := bytes.Repeat([]byte("grok-egress-"), 3000) // spans several chunks
	go func() { _, _ = conn.Write(payload) }()
	got := make([]byte, len(payload))
	_, err = io.ReadFull(conn, got)
	testutil.NoError(t, err)
	testutil.True(t, bytes.Equal(got, payload), "payload must round-trip intact")
}

func selfSignedTLS(t *testing.T) *tls.Config {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	testutil.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "tunnel.test"}, DNSNames: []string{"tunnel.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	testutil.NoError(t, err)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

func TestTrojanOverTLSRoundTrips(t *testing.T) {
	target := echoServer(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", selfSignedTLS(t))
	testutil.NoError(t, err)
	sum := sha256.Sum224([]byte("trojan-secret"))
	want := hex.EncodeToString(sum[:])
	addr := serve(t, ln, func(conn net.Conn) {
		head := make([]byte, 56+2+1)
		if _, err := io.ReadFull(conn, head); err != nil || string(head[:56]) != want || head[58] != 1 {
			return
		}
		dest := readTarget(t, conn, 1, 3, 4, false)
		var crlf [2]byte
		_, _ = io.ReadFull(conn, crlf[:])
		relay(dest, conn, conn)
	})
	host, port, _ := net.SplitHostPort(addr)
	roundTrip(t, "trojan://trojan-secret@"+host+":"+port+"?sni=tunnel.test&allowInsecure=1#t", target)
}

func TestVLESSOverWebSocketRoundTrips(t *testing.T) {
	target := echoServer(t)
	upgrader := websocket.Upgrader{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	testutil.NoError(t, err)
	id, _ := parseUUID(tunnelTestUUID)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tunnel" {
			http.NotFound(w, r)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conn := &wsConn{Conn: ws}
		defer conn.Close()
		head := make([]byte, 1+16+1+1)
		if _, err := io.ReadFull(conn, head); err != nil || head[0] != 0 || !bytes.Equal(head[1:17], id) || head[18] != 1 {
			return
		}
		dest := readTarget(t, conn, 1, 2, 3, true)
		_, _ = conn.Write([]byte{0, 0})
		relay(dest, conn, conn)
	})}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	roundTrip(t, "vless://"+tunnelTestUUID+"@"+host+":"+port+"?security=none&type=ws&path=%2Ftunnel#v", target)
}

func TestShadowsocksAEADRoundTrips(t *testing.T) {
	for _, method := range []string{"aes-256-gcm", "chacha20-ietf-poly1305"} {
		t.Run(method, func(t *testing.T) {
			target := echoServer(t)
			c, err := newSSCipher(method, "ss-password")
			testutil.NoError(t, err)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			testutil.NoError(t, err)
			addr := serve(t, ln, func(conn net.Conn) {
				// The server side of an ssConn: it reads the client's salt and
				// writes its own, exactly like the client in reverse.
				salt := make([]byte, c.keyLen)
				_, _ = rand.Read(salt)
				aead, _ := c.session(salt)
				ss := &ssConn{Conn: conn, cipher: c, writer: aead, writeNonce: make([]byte, aead.NonceSize())}
				if _, err := conn.Write(salt); err != nil {
					return
				}
				dest := readTarget(t, ss, 1, 3, 4, false)
				relay(dest, ss, ss)
			})
			credentials := base64.RawURLEncoding.EncodeToString([]byte(method + ":ss-password"))
			roundTrip(t, "ss://"+credentials+"@"+addr+"#s", target)
		})
	}
}

func TestVMessAEADRoundTrips(t *testing.T) {
	for _, security := range []string{"aes-128-gcm", "chacha20-poly1305"} {
		t.Run(security, func(t *testing.T) {
			target := echoServer(t)
			id, _ := parseUUID(tunnelTestUUID)
			cmdKey := vmessCmdKey(id)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			testutil.NoError(t, err)
			addr := serve(t, ln, func(conn net.Conn) { vmessServe(t, conn, cmdKey, target) })
			host, port, _ := net.SplitHostPort(addr)
			doc, _ := json.Marshal(map[string]string{"v": "2", "ps": "m", "add": host, "port": port, "id": tunnelTestUUID, "aid": "0", "scy": security, "net": "tcp"})
			roundTrip(t, "vmess://"+base64.StdEncoding.EncodeToString(doc), target)
		})
	}
}

// vmessServe is the server side of VMess AEAD.
func vmessServe(t *testing.T, conn net.Conn, cmdKey []byte, _ string) {
	authID := make([]byte, 16)
	if _, err := io.ReadFull(conn, authID); err != nil {
		return
	}
	block, _ := aes.NewCipher(vmessKDF(cmdKey, []byte("AES Auth ID Encryption"))[:16])
	plain := make([]byte, 16)
	block.Decrypt(plain, authID)
	if crc32.ChecksumIEEE(plain[:12]) != binary.BigEndian.Uint32(plain[12:]) {
		t.Error("vmess server: auth ID checksum mismatch")
		return
	}
	sealedLength := make([]byte, 18)
	nonce := make([]byte, 8)
	_, _ = io.ReadFull(conn, sealedLength)
	_, _ = io.ReadFull(conn, nonce)
	length, err := aesGCM(vmessKDF(cmdKey, []byte("VMess Header AEAD Key_Length"), authID, nonce)[:16]).Open(nil, vmessKDF(cmdKey, []byte("VMess Header AEAD Nonce_Length"), authID, nonce)[:12], sealedLength, authID)
	if err != nil {
		t.Error("vmess server: header length did not open")
		return
	}
	sealed := make([]byte, int(binary.BigEndian.Uint16(length))+16)
	_, _ = io.ReadFull(conn, sealed)
	header, err := aesGCM(vmessKDF(cmdKey, []byte("VMess Header AEAD Key"), authID, nonce)[:16]).Open(nil, vmessKDF(cmdKey, []byte("VMess Header AEAD Nonce"), authID, nonce)[:12], sealed, authID)
	if err != nil {
		t.Error("vmess server: header did not open")
		return
	}
	checksum := fnv.New32a()
	checksum.Write(header[:len(header)-4])
	if !bytes.Equal(checksum.Sum(nil), header[len(header)-4:]) {
		t.Error("vmess server: header checksum mismatch")
		return
	}
	bodyIV, bodyKey, respAuth, option, security := header[1:17], header[17:33], header[33], header[34], header[35]&0x0f
	padding := int(header[35] >> 4)
	if header[0] != 1 || option&(vmessOptChunkStream|vmessOptChunkMasking) != vmessOptChunkStream|vmessOptChunkMasking || header[37] != 1 {
		t.Error("vmess server: unexpected header fields")
		return
	}
	dest := readTarget(t, bytes.NewReader(header[38:len(header)-4-padding]), 1, 2, 3, true)

	readAEAD, _ := vmessBodyAEAD(security, bodyKey)
	respKey := sha256.Sum256(bodyKey)
	respIV := sha256.Sum256(bodyIV)
	writeAEAD, _ := vmessBodyAEAD(security, respKey[:16])
	// The server writes the response header, then its body chunks.
	respHeader := []byte{respAuth, 0, 0, 0}
	var lengthBytes [2]byte
	binary.BigEndian.PutUint16(lengthBytes[:], uint16(len(respHeader)))
	out := aesGCM(vmessKDF(respKey[:16], []byte("AEAD Resp Header Len Key"))[:16]).Seal(nil, vmessKDF(respIV[:16], []byte("AEAD Resp Header Len IV"))[:12], lengthBytes[:], nil)
	out = aesGCM(vmessKDF(respKey[:16], []byte("AEAD Resp Header Key"))[:16]).Seal(out, vmessKDF(respIV[:16], []byte("AEAD Resp Header IV"))[:12], respHeader, nil)
	if _, err := conn.Write(out); err != nil {
		return
	}
	// Reuse the client's chunk codec with the directions swapped.
	server := &vmessConn{Conn: conn, writer: newVMessChunks(writeAEAD, respIV[:16]), reader: newVMessChunks(readAEAD, bodyIV)}
	server.headerOnce.Do(func() {})
	relay(dest, server, server)
}

func TestShareLinksParse(t *testing.T) {
	info, err := parseTunnelURL("vless://" + tunnelTestUUID + "@example.com:443?security=tls&sni=cdn.example.com&type=ws&path=%2Fws&host=cdn.example.com#jp")
	testutil.NoError(t, err)
	testutil.Equal(t, info.SNI, "cdn.example.com")
	testutil.Equal(t, info.Network, "ws")
	testutil.Equal(t, info.Name, "jp")
	legacy := "ss://" + base64.StdEncoding.EncodeToString([]byte("aes-128-gcm:pw@1.2.3.4:8388")) + "#old"
	info, err = parseTunnelURL(legacy)
	testutil.NoError(t, err)
	testutil.Equal(t, info.Method, "aes-128-gcm")
	testutil.Equal(t, info.Host, "1.2.3.4")
	_, err = parseTunnelURL("ss://" + base64.StdEncoding.EncodeToString([]byte("rc4-md5:pw")) + "@1.2.3.4:8388")
	testutil.Error(t, err)
	masked := MaskProxy("trojan://supersecret@hk.example.com:443#hk")
	testutil.Equal(t, masked, "trojan://hk.example.com:443")
}
