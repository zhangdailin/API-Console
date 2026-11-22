package doubao

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

type conversationIdentity struct {
	deviceID, webID, teaUUID string
	scope                    string // Empty for anonymous requests.
}

// The persisted QR-login tuple is random session seed material. Deriving a
// pseudorandom tuple per owner/conversation keeps it stable across request-local
// clients and process restarts without storing raw caller conversation IDs.
// This is an identifier, never an authentication or authorization mechanism.
func (c *Client) bindConversationIdentity(ctx context.Context, downstreamID string) error {
	downstreamID = strings.TrimSpace(downstreamID)
	if downstreamID == "" {
		device, web, tea, err := NewIdentity()
		if err != nil {
			return err
		}
		c.dialogIdentity = &conversationIdentity{deviceID: device, webID: web, teaUUID: tea}
		return nil
	}
	acc := c.acc
	seed, _ := json.Marshal([]string{acc.DoubaoDeviceID, acc.DoubaoWebID, acc.DoubaoTeaUUID})
	if acc.DoubaoDeviceID == "" || acc.DoubaoWebID == "" {
		// Compatibility for direct/legacy clients without a QR identity tuple.
		// Proper QR accounts do not derive identity from rotating Cookies.
		version := store.DoubaoSessionVersion(acc)
		seed = version[:]
	}
	owner := middleware.APIKeyFingerprint(ctx)
	input, _ := json.Marshal([]any{"doubao.conversation.identity.v1", acc.ID, owner, downstreamID})
	mac := hmac.New(sha256.New, seed)
	_, _ = mac.Write(input)
	raw := mac.Sum(nil)
	device, web := identityDigits(raw[:8]), identityDigits(raw[8:16])
	c.dialogIdentity = &conversationIdentity{deviceID: device, webID: web, teaUUID: web, scope: "dialog:" + hex.EncodeToString(raw)}
	return nil
}

func identityDigits(raw []byte) string {
	n := new(big.Int).SetUint64(binary.BigEndian.Uint64(raw))
	base := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	span := new(big.Int).Mul(big.NewInt(9), base)
	return n.Mod(n, span).Add(n, base).String()
}

// challengeAccount describes the identity actually sent, while c.acc retains
// the persisted account generation used for Cookie commits and conflict guards.
func (c *Client) challengeAccount() *store.Account {
	acc := c.acc.Clone()
	if acc != nil && c.dialogIdentity != nil {
		acc.DoubaoDeviceID = c.dialogIdentity.deviceID
		acc.DoubaoWebID = c.dialogIdentity.webID
		acc.DoubaoTeaUUID = c.dialogIdentity.teaUUID
	}
	return acc
}
