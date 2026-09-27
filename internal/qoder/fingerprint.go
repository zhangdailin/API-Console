package qoder

// 稳定设备指纹派生
//
// 以账号身份单向哈希派生固定的伪物理设备特征，确保：
//   - 同一账号长期稳定：出站请求永远来自同一台虚拟设备，规避机器码漂移风控
//   - 多账号天然隔离：不同账号机器码彼此独立，阻断跨账号关联检测
//
// 与 qoder2api 的 internal/cosy/fingerprint.go 在「无盐种子」下逐字节同构
//（同一 seed 产出相同值）：
//   - machineId  : md5("machine:{seed}")      → 32 位十六进制
//   - machineType: md5("machinetype:{seed}")  → 32 位十六进制截 18 位
//   - machineToken: sha512("machinetoken:{seed}") → base64url 截 43 位
//
// 本机盐（install salt）：可选混入所有派生种子。纯 seed 派生的问题是「知道
// seed 即可算出指纹，且所有同源部署派生值完全相同」——上游一旦识别派生模式
// 可全局拉黑。设置本机盐后，每个部署拥有独立的指纹空间；盐为空时保持兼容
//（交叉校验向量成立）。盐生成后必须保持不变：变更 salt 即指纹整体漂移，
// 等价于换设备。

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// InstallSaltSettingKey is the settings key that holds this deployment's
// fingerprint salt. It is written once, on the first start that finds it empty.
const InstallSaltSettingKey = "qoder.machine_salt"

var (
	installSaltMu   sync.RWMutex
	installSalt     string
	installSaltOnce sync.Mutex
)

// SetInstallSalt sets this deployment's fingerprint salt. An empty value keeps
// the derivation byte-identical to the reference implementation, which is what
// the cross-check vectors assert.
func SetInstallSalt(salt string) {
	installSaltMu.Lock()
	installSalt = strings.TrimSpace(salt)
	installSaltMu.Unlock()
}

// InstallSalt returns the salt currently in effect. Empty means the
// reference-compatible derivation.
func InstallSalt() string {
	installSaltMu.RLock()
	defer installSaltMu.RUnlock()
	return installSalt
}

// SettingsStore is the subset of the account store needed to persist the
// install salt. It is satisfied by *store.Store.
type SettingsStore interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// EnsureInstallSalt reads (or, on the first start, generates and persists) this
// deployment's fingerprint salt.
//
// The salt must never rotate: every derived fingerprint would move with it,
// which the upstream sees as every device changing at once. A store failure is
// therefore not fatal — the process falls back to the reference derivation
// rather than silently minting a new random salt on each start.
func EnsureInstallSalt(ctx context.Context, s SettingsStore) string {
	installSaltOnce.Lock()
	defer installSaltOnce.Unlock()
	if current := InstallSalt(); current != "" {
		return current
	}
	if s == nil {
		return ""
	}
	stored, err := s.GetSetting(ctx, InstallSaltSettingKey)
	if err != nil {
		// A failed read is not proof that no salt exists. Never overwrite an
		// identity that may already be persisted with newly generated material.
		return ""
	}
	if stored = strings.TrimSpace(stored); stored != "" {
		SetInstallSalt(stored)
		return stored
	}
	generated, err := newInstallSalt()
	if err != nil {
		return ""
	}
	if err := s.SetSetting(ctx, InstallSaltSettingKey, generated); err != nil {
		return ""
	}
	// Only durable material may become this process's device identity.
	SetInstallSalt(generated)
	return generated
}

func newInstallSalt() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// saltedSeed mixes the install salt into a derivation seed. An empty salt
// returns the seed unchanged, which keeps the reference vectors valid.
func saltedSeed(seed string) string {
	installSaltMu.RLock()
	salt := installSalt
	installSaltMu.RUnlock()
	if salt == "" {
		return seed
	}
	return seed + "|salt:" + salt
}

// FingerprintSeed picks the derivation seed: the account UID when it is known,
// otherwise the credential. The fallback keeps an account whose identity has
// not been resolved yet stable across restarts instead of drifting.
func FingerprintSeed(uid, credential string) string {
	uid = strings.TrimSpace(uid)
	if uid != "" {
		return uid
	}
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return ""
	}
	return "cred:" + credential
}

func deriveID(seed, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + saltedSeed(seed)))
	return fmt.Sprintf("%x", sum)
}

// DeriveMachineID derives the 32-hex device id.
func DeriveMachineID(seed string) string {
	if seed == "" {
		return ""
	}
	return deriveID(seed, "machine")
}

// DeriveMachineType derives the 18-character device type.
func DeriveMachineType(seed string) string {
	if seed == "" {
		return ""
	}
	return deriveID(seed, "machinetype")[:18]
}

// DeriveMachineToken derives the 43-character device token.
func DeriveMachineToken(seed string) string {
	if seed == "" {
		return ""
	}
	sum := sha512.Sum512([]byte("machinetoken:" + saltedSeed(seed)))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:43]
}

// DeviceFingerprint is the trio of device headers derived for one account.
type DeviceFingerprint struct {
	// MachineID stays the identity the login was authorized under: it is bound
	// to the credential, so it is carried through unchanged rather than
	// re-derived. Deriving it would break the binding and every request would
	// be signed by a device the token does not know.
	MachineID string
	// Token and Type are not part of that binding, and are what the upstream
	// actually fingerprints — so they are derived, stable, and distinct from
	// the id, the way a real CLI device presents them.
	Token string
	Type  string
}

// FingerprintFor derives the device trio for one account.
//
// MachineID is passed through as recorded; only the token and the type are
// derived, from the same seed (UID, else the device id, else the credential).
// A seed that cannot be built leaves the derived pair empty and the caller
// falls back to the values the protocol already used.
func FingerprintFor(machineID, uid, credential string) DeviceFingerprint {
	seed := FingerprintSeed(uid, credential)
	if seed == "" {
		seed = FingerprintSeed("", machineID)
	}
	return DeviceFingerprint{
		MachineID: strings.TrimSpace(machineID),
		Token:     DeriveMachineToken(seed),
		Type:      DeriveMachineType(seed),
	}
}
