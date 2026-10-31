package grok

import (
	"context"
	"crypto/sha1"
	"fmt"
	"net/http"
	"strings"
)

// conversationGroupNamespace mirrors xai-grok-shell CONVERSATION_GROUP_NAMESPACE.
const conversationGroupNamespace = "xai:grok-build:conversation-group:"

// uuidNamespaceOID is RFC 4122's NameSpace_OID.
var uuidNamespaceOID = [16]byte{0x6b, 0xa7, 0xb8, 0x12, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}

// conversationGroupID reproduces grok-build derive_conversation_group_id:
// UUIDv5(NameSpace_OID, namespace + root session id). The gateway treats each
// stable session as its own root conversation, so the group derives from the
// same id sent as x-grok-conv-id.
func conversationGroupID(rootSessionID string) string {
	rootSessionID = strings.TrimSpace(rootSessionID)
	if rootSessionID == "" {
		return ""
	}
	return uuidV5(uuidNamespaceOID, conversationGroupNamespace+rootSessionID)
}

// uuidV5 is the RFC 4122 name-based UUID over SHA-1.
func uuidV5(namespace [16]byte, name string) string {
	hash := sha1.New()
	hash.Write(namespace[:])
	hash.Write([]byte(name))
	sum := hash.Sum(nil)
	var uuid [16]byte
	copy(uuid[:], sum[:16])
	uuid[6] = (uuid[6] & 0x0f) | 0x50
	uuid[8] = (uuid[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16])
}

type clientTurnIndexKey struct{}

// withClientTurnIndex keeps the official client's x-grok-turn-idx for the
// upstream call. Only a value the client sent is kept: the gateway cannot
// recover the CLI's prompt index from a stateless request and never invents one.
func withClientTurnIndex(ctx context.Context, r *http.Request) context.Context {
	if r == nil {
		return ctx
	}
	if turn := normalizeTurnIndex(r.Header.Get("x-grok-turn-idx")); turn != "" {
		return context.WithValue(ctx, clientTurnIndexKey{}, turn)
	}
	return ctx
}

func clientTurnIndex(ctx context.Context) string {
	turn, _ := ctx.Value(clientTurnIndexKey{}).(string)
	return turn
}

// normalizeTurnIndex accepts only a non-negative decimal u64.
func normalizeTurnIndex(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 20 {
		return ""
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return ""
		}
	}
	if len(value) == 20 && value > "18446744073709551615" {
		return ""
	}
	return value
}
