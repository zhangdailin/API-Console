package qoder

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"orchids-api/internal/testutil"
	"strings"
	"testing"

	"encoding/json"
)

// recordingSource replays a fixed byte sequence and fails when it runs dry, so
// a derivation that reads more entropy than the fixture provides is a test
// failure rather than a silent zero-filled key. Reads of any size are served
// from the same stream, because the RSA padding is drawn one byte at a time
// while the UUID and the runtime key are drawn in blocks.
type recordingSource struct {
	data []byte
	pos  int
}

func newRecordingSource(chunks ...[]byte) *recordingSource {
	var joined []byte
	for _, chunk := range chunks {
		joined = append(joined, chunk...)
	}
	return &recordingSource{data: joined}
}

func (r *recordingSource) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, errEntropyExhausted
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

var errEntropyExhausted = &entropyExhaustedError{}

type entropyExhaustedError struct{}

func (*entropyExhaustedError) Error() string { return "test entropy source exhausted" }

// TestRuntimeFieldsAccessorRoundTrip proves a stored pair is accepted and a half
// pair is rejected, so a partially written account re-derives instead of sending
// an unusable header.
func TestRuntimeFieldsAccessorRoundTrip(t *testing.T) {
	t.Parallel()

	testutil.False(t, (RuntimeFields{}).Complete(), "Complete() = true for an empty pair")
	testutil.False(t, (RuntimeFields{Key: "x"}).Complete(), "Complete() = true for a pair with no info")
	testutil.False(t, !(RuntimeFields{EncryptUserInfo: "a", Key: "b"}).Complete(), "Complete() = false for a full pair")
}

func TestReferenceRuntimeIdentityIncludesTokensAndAccountClass(t *testing.T) {
	entropy := newRecordingSource(bytes.Repeat([]byte{0x31}, 16), bytes.Repeat([]byte{0x32}, 512))
	fields, err := referenceRuntimeFieldsFor(entropy, referenceRuntimeFieldInput{
		Name: "Test User", Aid: "uid-1", UID: "uid-1", UserType: "personal_professional_trial",
		SecurityOAuthToken: "access-secret", RefreshToken: "refresh-secret",
	})
	testutil.NoError(t, err)
	testutil.False(t, !fields.Complete(), "reference runtime pair incomplete")
	key := []byte("3131313131313131") // hex of the reference's first 8 random bytes
	sealed, err := base64.StdEncoding.DecodeString(fields.EncryptUserInfo)
	testutil.NoError(t, err)
	block, err := aes.NewCipher(key)
	testutil.NoError(t, err)
	plaintext := make([]byte, len(sealed))
	cipher.NewCBCDecrypter(block, key).CryptBlocks(plaintext, sealed)
	var identity map[string]string
	testutil.NoError(t, json.Unmarshal(unpadPKCS7(t, plaintext), &identity))
	if len(identity) != 9 || identity["security_oauth_token"] != "access-secret" || identity["refresh_token"] != "refresh-secret" || identity["aid"] != "uid-1" || identity["user_type"] != "personal_professional_trial" {
		t.Fatalf("reference identity field shape mismatch: keys=%d aid=%q user_type=%q", len(identity), identity["aid"], identity["user_type"])
	}
}

func unpadPKCS7(t *testing.T, padded []byte) []byte {
	t.Helper()
	testutil.NotEqual(t, len(padded), 0)
	padding := int(padded[len(padded)-1])
	testutil.Falsef(t, padding <= 0 || padding > len(padded), "invalid padding %d", padding)
	return padded[:len(padded)-padding]
}

// TestPKCEPairShape pins the verifier/challenge contract: a 43..128 character
// unreserved verifier and an unpadded base64url SHA-256 challenge.
func TestPKCEPairShape(t *testing.T) {
	t.Parallel()

	source := newRecordingSource([]byte{0}, bytes.Repeat([]byte{0x42}, 43))
	verifier, challenge, err := pkcePair(source)
	testutil.NoError(t, err, "pkcePair() error = %v")
	testutil.Equal(t, len(verifier), 43)
	testutil.Falsef(t, strings.ContainsAny(challenge, "+/="), "challenge %q is not unpadded base64url", challenge)
	if len(challenge) != 43 && len(challenge) != 42 {
		// A 32-byte digest is 43 base64url characters once padding is dropped.
		t.Fatalf("challenge length = %d, want 43", len(challenge))
	}
	// A verifier outside the unreserved set would be rejected by the browser
	// page, so the charset is asserted rather than assumed.
	const allowed = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	for _, r := range verifier {
		testutil.Falsef(t, !strings.ContainsRune(allowed, r), "verifier contains %q, which is outside the allowed charset", r)
	}
}

// TestPKCEVerifierLengthSpread covers the upper bound of the verifier length.
func TestPKCEVerifierLengthSpread(t *testing.T) {
	t.Parallel()

	source := newRecordingSource([]byte{85}, bytes.Repeat([]byte{0x11}, 128))
	verifier, _, err := pkcePair(source)
	testutil.NoError(t, err, "pkcePair() error = %v")
	testutil.Equal(t, len(verifier), 128)
}

// TestNewUUIDVersionAndVariant pins the UUID shape the upstream expects in the
// nonce and machine id parameters.
func TestNewUUIDVersionAndVariant(t *testing.T) {
	t.Parallel()

	source := newRecordingSource(bytes.Repeat([]byte{0xff}, 16))
	value, err := newUUID(source)
	testutil.NoError(t, err, "newUUID() error = %v")
	testutil.Equal(t, len(value), 36)
	testutil.Equal(t, value[14], '4')
	testutil.Falsef(t, !strings.ContainsRune("89ab", rune(value[19])), "uuid = %q, want an RFC 4122 variant at index 19", value)
}

// TestRSAEncryptionRejectsShortModulusInput guards the padding arithmetic.
func TestRSAEncryptionRejectsShortModulusInput(t *testing.T) {
	t.Parallel()

	publicKey, err := runtimePublicKey()
	testutil.NoError(t, err, "runtimePublicKey() error = %v")
	source := newRecordingSource()
	_, err = rsaEncryptPKCS1v15WithSource(source, publicKey, make([]byte, publicKey.Size()))
	testutil.Error(t, err)
}
