package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/goccy/go-json"
	"github.com/redis/go-redis/v9"
)

const encryptedCredentialPrefix = "enc:v1:"

type credentialCipher struct {
	aead cipher.AEAD
}

func newCredentialCipher(key []byte) (*credentialCipher, error) {
	if len(key) == 0 {
		return nil, nil
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("credential encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &credentialCipher{aead: aead}, nil
}

func (c *credentialCipher) encrypt(value string) (string, error) {
	if c == nil || value == "" {
		return value, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(value), nil)
	return encryptedCredentialPrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

func (c *credentialCipher) decrypt(value string) (string, error) {
	if value == "" || !strings.HasPrefix(value, encryptedCredentialPrefix) {
		return value, nil
	}
	if c == nil {
		return "", fmt.Errorf("encrypted credential found but no encryption key is configured")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(value, encryptedCredentialPrefix))
	if err != nil {
		return "", fmt.Errorf("decode encrypted credential: %w", err)
	}
	if len(raw) < c.aead.NonceSize() {
		return "", fmt.Errorf("encrypted credential is truncated")
	}
	nonce, ciphertext := raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt credential: %w", err)
	}
	return string(plain), nil
}

func accountCredentialFields(acc *Account) []*string {
	return []*string{
		&acc.SessionID,
		&acc.ClientCookie,
		&acc.RefreshToken,
		&acc.SessionCookie,
		&acc.ClientUat,
		&acc.Token,
		&acc.OAuthAccessToken,
		&acc.OAuthRefreshToken,
		&acc.WorkBuddyAccessToken,
		&acc.WorkBuddyRefreshToken,
		&acc.QoderAccessToken,
		&acc.QoderRefreshToken,
		&acc.QoderRuntimeInfo,
		&acc.QoderRuntimeKey,
		&acc.ClineAccessToken,
		&acc.ClineRefreshToken,
	}
}

func (s *redisStore) marshalAccount(acc *Account) ([]byte, error) {
	if acc == nil {
		return json.Marshal(acc)
	}
	stored := *acc
	for _, field := range accountCredentialFields(&stored) {
		encrypted, err := s.credentials.encrypt(*field)
		if err != nil {
			return nil, err
		}
		*field = encrypted
	}
	return json.Marshal(&stored)
}

func (s *redisStore) unmarshalAccount(data []byte, fallbackID int64) (*Account, error) {
	var acc Account
	if err := json.Unmarshal(data, &acc); err != nil {
		return nil, err
	}
	for _, field := range accountCredentialFields(&acc) {
		plain, err := s.credentials.decrypt(*field)
		if err != nil {
			return nil, err
		}
		*field = plain
	}
	if acc.ID == 0 {
		acc.ID = fallbackID
	}
	return &acc, nil
}

func (s *redisStore) migrateLegacyAccountCredentials(ctx context.Context) error {
	if s == nil {
		return nil
	}
	ids, err := s.client.SMembers(ctx, s.accountsIDsKey()).Result()
	if err != nil {
		return err
	}
	for _, idText := range ids {
		id, err := strconv.ParseInt(strings.TrimSpace(idText), 10, 64)
		if err != nil || id == 0 {
			continue
		}
		key := s.accountsKey(id)
		raw, err := s.client.Get(ctx, key).Bytes()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			return err
		}
		legacy, err := hasLegacyCredential(raw)
		if err != nil {
			return fmt.Errorf("decode account %d during credential migration: %w", id, err)
		}
		acc, err := s.unmarshalAccount(raw, id)
		if err != nil {
			return fmt.Errorf("decrypt account %d during credential migration: %w", id, err)
		}
		if !legacy || s.credentials == nil {
			continue
		}
		encoded, err := s.marshalAccount(acc)
		if err != nil {
			return fmt.Errorf("encrypt account %d during credential migration: %w", id, err)
		}
		if err := s.client.Set(ctx, key, encoded, 0).Err(); err != nil {
			return err
		}
	}
	return nil
}

func hasLegacyCredential(data []byte) (bool, error) {
	var values map[string]interface{}
	if err := json.Unmarshal(data, &values); err != nil {
		return false, err
	}
	for _, name := range []string{
		"session_id", "client_cookie", "refresh_token",
		"session_cookie", "client_uat", "token", "oauth_access_token", "oauth_refresh_token",
		"workbuddy_access_token", "workbuddy_refresh_token",
		"qoder_access_token", "qoder_refresh_token", "qoder_runtime_info", "qoder_runtime_key",
	} {
		value, _ := values[name].(string)
		if value != "" && !strings.HasPrefix(value, encryptedCredentialPrefix) {
			return true, nil
		}
	}
	return false, nil
}
