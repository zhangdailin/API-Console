package qoder

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sync"

	"encoding/json"
)

// The gateway reads two things from a chat request that are easy to get wrong.
//
// Body: the JSON is not sent raw. It is standard-base64/base64-encoded with a
// private alphabet and padding character, and then the two outer thirds are
// swapped. The signature is computed over the encoded form, so an
// implementation that signs the JSON is rejected with an auth error that looks
// like a bad token.
//
// Authorization: "Bearer COSY.<payload>.<signature>" where the signature is the
// MD5 of five newline-separated fields, and the covered path is the request
// path without the /algo prefix and without the query string.

// bodyAlphabet is the private base64 alphabet the gateway expects. The
// character set is fixed; it is not chosen here.
const bodyAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

// bodyPadding substitutes for '=' in the private alphabet.
const bodyPadding = '$'

// bodyEncoding is the strict private-alphabet encoder.
var bodyEncoding = base64.NewEncoding(bodyAlphabet).WithPadding(bodyPadding).Strict()

type bodyJSONWriter struct {
	buffer  bytes.Buffer
	encoder *json.Encoder
}

var bodyJSONWriters = sync.Pool{New: func() interface{} {
	w := &bodyJSONWriter{}
	w.buffer.Grow(4096)
	w.encoder = json.NewEncoder(&w.buffer)
	return w
}}

// Only the intermediate JSON buffer is reused. The returned encoded body owns
// its bytes, including while net/http may still be sending the request body.
func marshalEncodedBody(value interface{}) ([]byte, error) {
	w := bodyJSONWriters.Get().(*bodyJSONWriter)
	defer func() {
		clear(w.buffer.Bytes())
		w.buffer.Reset()
		if w.buffer.Cap() <= 64*1024 {
			bodyJSONWriters.Put(w)
		}
	}()
	if err := w.encoder.Encode(value); err != nil {
		return nil, err
	}
	raw := w.buffer.Bytes()
	return EncodeBody(raw[:len(raw)-1]), nil // Encoder appends exactly one LF.
}

// EncodeBody renders the JSON body the way the wire format requires.
//
// bodyEncoding already substitutes the private alphabet (and the '$' padding),
// so only the third swap remains. The swap is a positional reordering over the
// already-encoded bytes, which is why the signature can be computed over the
// result before it is sent.
func EncodeBody(raw []byte) []byte {
	encoded := make([]byte, bodyEncoding.EncodedLen(len(raw)))
	bodyEncoding.Encode(encoded, raw)
	q := len(encoded) / 3
	for i := range q {
		encoded[i], encoded[len(encoded)-q+i] = encoded[len(encoded)-q+i], encoded[i]
	}
	return encoded
}

// decodeBody reverses EncodeBody for the narrow case where a refreshed
// credential must replay an otherwise identical request with a fresh identity.
func decodeBody(encoded []byte) ([]byte, error) {
	// Base64's Strict mode still ignores CR/LF. Signed bodies must retain their
	// exact framing, so reject line breaks before the positional swap.
	if bytes.ContainsAny(encoded, "\r\n") {
		return nil, fmt.Errorf("encoded body contains a line break")
	}
	unswapped := swapOuterThirds(encoded)
	decoded := make([]byte, bodyEncoding.DecodedLen(len(unswapped)))
	n, err := bodyEncoding.Decode(decoded, unswapped)
	if err != nil {
		return nil, err
	}
	return decoded[:n], nil
}

// swapOuterThirds moves the leading third to the end and the trailing third to
// the front, leaving the middle in place. The middle section absorbs any
// remainder so that all bytes are covered exactly once.
func swapOuterThirds(src []byte) []byte {
	q := len(src) / 3
	out := make([]byte, 0, len(src))
	out = append(out, src[len(src)-q:]...)
	out = append(out, src[q:len(src)-q]...)
	out = append(out, src[:q]...)
	return out
}

// cosyPayload is the decoded COSY payload. Field order and the empty
// ideVersion are part of the wire contract: the payload is base64-encoded
// verbatim, so any difference changes every signature.
type cosyPayload struct {
	Version     string `json:"version"`
	RequestID   string `json:"requestId"`
	Info        string `json:"info"`
	CosyVersion string `json:"cosyVersion"`
	IDEVersion  string `json:"ideVersion"`
}

func cosySignature(payloadBase64 []byte, runtimeKey, unixSeconds string, encodedBody []byte, signedPath string) [md5.Size]byte {
	hash := md5.New()
	_, _ = hash.Write(payloadBase64)
	for _, field := range [...]string{runtimeKey, unixSeconds} {
		_, _ = hash.Write([]byte{'\n'})
		_, _ = hash.Write([]byte(field))
	}
	_, _ = hash.Write([]byte{'\n'})
	_, _ = hash.Write(encodedBody)
	_, _ = hash.Write([]byte{'\n'})
	_, _ = hash.Write([]byte(signedPath))
	var sum [md5.Size]byte
	hash.Sum(sum[:0])
	return sum
}

// Build the per-request signed bearer in one owned string. Neither the nonce
// nor signature is cached; only the intermediate JSON writer is reused.
func buildCOSYAuthorization(requestID, info, version, runtimeKey, seconds string, body []byte, path string) (string, error) {
	w := bodyJSONWriters.Get().(*bodyJSONWriter)
	defer func() {
		clear(w.buffer.Bytes())
		w.buffer.Reset()
		if w.buffer.Cap() <= 64*1024 {
			bodyJSONWriters.Put(w)
		}
	}()
	if err := w.encoder.Encode(cosyPayload{Version: "v1", RequestID: requestID, Info: info, CosyVersion: version}); err != nil {
		return "", err
	}
	raw := w.buffer.Bytes()
	raw = raw[:len(raw)-1]
	const prefix = "Bearer COSY."
	encodedLen := base64.StdEncoding.EncodedLen(len(raw))
	total := len(prefix) + encodedLen + 1 + md5.Size*2
	var scratch [4096]byte
	var bearer []byte
	if total <= len(scratch) {
		bearer = scratch[:total]
	} else {
		bearer = make([]byte, total)
	}
	copy(bearer, prefix)
	payload := bearer[len(prefix) : len(prefix)+encodedLen]
	base64.StdEncoding.Encode(payload, raw)
	sum := cosySignature(payload, runtimeKey, seconds, body, path)
	bearer[len(prefix)+encodedLen] = '.'
	hex.Encode(bearer[len(prefix)+encodedLen+1:], sum[:])
	return string(bearer), nil
}
