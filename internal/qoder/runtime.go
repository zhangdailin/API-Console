package qoder

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"

	"encoding/json"
)

// The gateway does not accept the raw OAuth token as its request credential.
// Every request carries a locally derived, per-account pair:
//
//	Cosy-Key          RSA(PKCS#1 v1.5) encryption of a fresh 16-byte AES key
//	Authorization     "Bearer COSY.<payload>.<signature>", where payload.info is
//	                  that same AES key encrypting the account's runtime fields
//
// The runtime fields themselves are the AES plaintext, and the CLI reuses the
// pair it generated at login instead of regenerating it per request. This
// package does the same: the pair is derived once per account and stored beside
// the credential.
//
// This is a protocol reproduction, not a security design. It is reproduced here
// because the gateway rejects anything else.

// runtimePublicKeyPEM is the gateway's runtime field public key. It is a
// non-secret public key: it is distributed inside the Qoder CLI and only ever
// encrypts material the client itself generated. The 1024-bit modulus is the
// upstream's choice, not a local decision.
const runtimePublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// RuntimeFields is the derived per-account authentication pair.
type RuntimeFields struct {
	// EncryptUserInfo is the AES-128-CBC ciphertext of the runtime field JSON,
	// standard base64. It travels inside the COSY payload.
	EncryptUserInfo string
	// Key is the RSA ciphertext of the AES key, standard base64. It travels in
	// the Cosy-Key header.
	Key string
}

// referenceRuntimeFieldInput matches the reference bridge's encrypted
// AuthIdentity. Values (including unknown metadata) are strings, and empty
// keys are serialized instead of omitted. Tokens must never be logged.
type referenceRuntimeFieldInput struct {
	Name               string `json:"name"`
	Aid                string `json:"aid"`
	UID                string `json:"uid"`
	YxUID              string `json:"yx_uid"`
	OrganizationID     string `json:"organization_id"`
	OrganizationName   string `json:"organization_name"`
	UserType           string `json:"user_type"`
	SecurityOAuthToken string `json:"security_oauth_token"`
	RefreshToken       string `json:"refresh_token"`
}

// referenceRuntimeFieldsFor uses the same random temp-key and encrypted
// identity layout as qoder2api's cosy.NewSession.
func referenceRuntimeFieldsFor(entropy source, in referenceRuntimeFieldInput) (RuntimeFields, error) {
	if entropy == nil {
		entropy = cryptoSource{}
	}
	var raw [16]byte
	if _, err := io.ReadFull(entropy, raw[:]); err != nil {
		return RuntimeFields{}, fmt.Errorf("read reference runtime entropy: %w", err)
	}
	key := []byte(hex.EncodeToString(raw[:])[:16])
	plaintext, err := json.Marshal(in)
	if err != nil {
		return RuntimeFields{}, fmt.Errorf("marshal reference runtime identity: %w", err)
	}
	sealed, err := aesCBCEncryptPKCS7(plaintext, key)
	if err != nil {
		return RuntimeFields{}, err
	}
	publicKey, err := runtimePublicKey()
	if err != nil {
		return RuntimeFields{}, err
	}
	wrapped, err := rsaEncryptPKCS1v15WithSource(entropy, publicKey, key)
	if err != nil {
		return RuntimeFields{}, fmt.Errorf("wrap reference runtime key: %w", err)
	}
	return RuntimeFields{EncryptUserInfo: base64.StdEncoding.EncodeToString(sealed), Key: base64.StdEncoding.EncodeToString(wrapped)}, nil
}

// source is the entropy seam. Both the UUID bytes and the RSA padding come from
// it, which is what makes the derivation reproducible under test.
type source interface {
	Read(p []byte) (int, error)
}

type cryptoSource struct{}

func (cryptoSource) Read(p []byte) (int, error) { return rand.Read(p) }

// formatUUID renders 16 bytes as a lowercase 8-4-4-4-12 UUID string.
func formatUUID(value [16]byte) string {
	var out [36]byte
	hex.Encode(out[0:8], value[0:4])
	hex.Encode(out[9:13], value[4:6])
	hex.Encode(out[14:18], value[6:8])
	hex.Encode(out[19:23], value[8:10])
	hex.Encode(out[24:36], value[10:16])
	out[8], out[13], out[18], out[23] = '-', '-', '-', '-'
	return string(out[:])
}

// aesCBCEncryptPKCS7 encrypts with AES-128-CBC, the key doubling as the IV, and
// applies PKCS#7 padding. The IV choice is upstream's; it is reproduced because
// the gateway recomputes nothing.
func aesCBCEncryptPKCS7(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("build runtime cipher: %w", err)
	}
	padded := pkcs7Pad(plaintext, block.BlockSize())
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key[:block.BlockSize()]).CryptBlocks(out, padded)
	return out, nil
}

func pkcs7Pad(plaintext []byte, blockSize int) []byte {
	padding := blockSize - len(plaintext)%blockSize
	out := make([]byte, len(plaintext)+padding)
	copy(out, plaintext)
	for i := len(plaintext); i < len(out); i++ {
		out[i] = byte(padding)
	}
	return out
}

// rsaEncryptPKCS1v15WithSource implements PKCS#1 v1.5 encryption with an
// explicit non-zero padding string. crypto/rsa's own primitive draws the
// padding from crypto/rand, which would make the derivation untestable; the
// padding is otherwise identical, and every candidate byte is rejected if zero.
func rsaEncryptPKCS1v15WithSource(entropy source, publicKey *rsa.PublicKey, message []byte) ([]byte, error) {
	size := publicKey.Size()
	if len(message) > size-11 {
		return nil, fmt.Errorf("runtime key is too long for the %d-byte modulus", size)
	}
	block := make([]byte, size)
	block[0] = 0x00
	block[1] = 0x02
	padding := size - len(message) - 3
	if padding < 8 {
		return nil, fmt.Errorf("runtime key leaves only %d bytes of padding", padding)
	}
	// A zero byte would terminate the padding string early, so every draw is
	// retried until it is non-zero.
	for i := 0; i < padding; {
		var one [1]byte
		if _, err := entropy.Read(one[:]); err != nil {
			return nil, err
		}
		if one[0] == 0 {
			continue
		}
		block[2+i] = one[0]
		i++
	}
	block[2+padding] = 0x00
	copy(block[2+padding+1:], message)

	encrypted := new(big.Int).Exp(new(big.Int).SetBytes(block), big.NewInt(int64(publicKey.E)), publicKey.N).Bytes()
	if len(encrypted) < size {
		padded := make([]byte, size-len(encrypted), size)
		encrypted = append(padded, encrypted...)
	}
	return encrypted, nil
}

var (
	runtimeKeyOnce sync.Once
	runtimeKey     *rsa.PublicKey
	runtimeKeyErr  error
)

// runtimePublicKey parses the pinned key once. A parse failure is a build or
// constant error, not a runtime condition, so it is kept and returned.
func runtimePublicKey() (*rsa.PublicKey, error) {
	runtimeKeyOnce.Do(func() {
		block, _ := pem.Decode([]byte(runtimePublicKeyPEM))
		if block == nil {
			runtimeKeyErr = fmt.Errorf("pinned runtime public key is not PEM")
			return
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			runtimeKeyErr = fmt.Errorf("parse pinned runtime public key: %w", err)
			return
		}
		key, ok := parsed.(*rsa.PublicKey)
		if !ok {
			runtimeKeyErr = fmt.Errorf("pinned runtime public key is %T, want RSA", parsed)
			return
		}
		runtimeKey = key
	})
	return runtimeKey, runtimeKeyErr
}

// pkcePair builds the device flow verifier and its S256 challenge. The verifier
// is 43..128 characters from the unreserved set, and the challenge is the
// unpadded base64url SHA-256 of it.
func pkcePair(entropy source) (verifier, challenge string, err error) {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	if entropy == nil {
		entropy = cryptoSource{}
	}
	var lengthByte [1]byte
	if _, err := entropy.Read(lengthByte[:]); err != nil {
		return "", "", fmt.Errorf("read pkce length entropy: %w", err)
	}
	length := 43 + int(lengthByte[0]%86)
	raw := make([]byte, length)
	if _, err := entropy.Read(raw); err != nil {
		return "", "", fmt.Errorf("read pkce verifier entropy: %w", err)
	}
	encoded := make([]byte, length)
	for i, b := range raw {
		encoded[i] = charset[int(b)%len(charset)]
	}
	verifier = string(encoded)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// newUUID mints a lowercase v4 UUID from the given entropy source.
func newUUID(entropy source) (string, error) {
	if entropy == nil {
		entropy = cryptoSource{}
	}
	var raw [16]byte
	if _, err := entropy.Read(raw[:]); err != nil {
		return "", fmt.Errorf("read uuid entropy: %w", err)
	}
	var value [16]byte
	copy(value[:], raw[:])
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return formatUUID(value), nil
}

// Three independent UUIDs share one entropy read, not one identity. Each UUID
// retains its own 122 random bits and the same v4/version wire format.
func newChatUUIDs(entropy source) (request, set, session string, err error) {
	if entropy == nil {
		entropy = cryptoSource{}
	}
	var raw [48]byte
	if _, err = io.ReadFull(entropy, raw[:]); err != nil {
		return "", "", "", fmt.Errorf("read uuid entropy: %w", err)
	}
	var ids [3]string
	for i := range ids {
		var value [16]byte
		copy(value[:], raw[i*16:(i+1)*16])
		value[6] = (value[6] & 0x0f) | 0x40
		value[8] = (value[8] & 0x3f) | 0x80
		ids[i] = formatUUID(value)
	}
	return ids[0], ids[1], ids[2], nil
}

// Complete reports whether the pair carries both halves. A half-populated pair
// is treated as absent: sending it would produce an auth failure that looks like
// a bad token instead of a missing derivation.
func (f RuntimeFields) Complete() bool {
	return strings.TrimSpace(f.EncryptUserInfo) != "" && strings.TrimSpace(f.Key) != ""
}
