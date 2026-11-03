package egress

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
)

// grokUARotation holds a small set of realistic Chrome macOS user agents. UA is
// bound to a fingerprint so a rotated UA is always paired with clearance solved
// under that same fingerprint.
var grokUARotation = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36",
}

// pickUserAgent returns a stable UA for a fingerprint. The same affinity key
// (account identity) maps to the same UA unless rotation is forced by clearing
// the fingerprint.
func pickUserAgent(affinity string) string {
	if affinity == "" {
		affinity = "default"
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(affinity))))
	return grokUARotation[uint32(binary.LittleEndian.Uint64(sum[:8]))%uint32(len(grokUARotation))]
}
