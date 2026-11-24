package grok

// Shared HTTP plumbing for Grok Build and account bookkeeping.

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// TokenFingerprint is a short, non-reversible digest of a credential, used in
// logs and refresh bookkeeping so a token itself never reaches a log line.
func TokenFingerprint(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return fmt.Sprintf("%x", sum[:8])
}

// credentialAffinity derives a stable, non-reversible key from an account
// identity, so per-credential state never holds the credential itself.
func credentialAffinity(identity string) string {
	normalized := strings.TrimSpace(identity)
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("cred_%x", sum[:16])
}
