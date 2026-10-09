package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"encoding/json"
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
	if value == "" {
		return value, nil
	}
	if !strings.HasPrefix(value, encryptedCredentialPrefix) {
		if c != nil {
			return "", fmt.Errorf("credential ciphertext is required")
		}
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
		&acc.ClientCookie,
		&acc.RefreshToken,
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

// validateAccountCredentials checks decryption without changing stored records.
func (s *redisStore) validateAccountCredentials(ctx context.Context) error {
	_, err := s.ListAccounts(ctx)
	return err
}
