package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"orchids-api/internal/util"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisSessionBackend keeps admin sessions in Redis so they survive a process
// restart (every deploy) and are shared by every replica behind the same
// database. Only a digest of the token is stored, so a Redis dump never
// exposes a usable session cookie.
type redisSessionBackend struct {
	client *redis.Client
	prefix string
}

func (b *redisSessionBackend) AdminCredentials(ctx context.Context, user, password string) (string, string, error) {
	raw, err := b.client.Get(ctx, b.prefix+"settings:config").Result()
	if err == redis.Nil {
		generation, checkErr := b.client.Exists(ctx, b.prefix+"admin:session_generation").Result()
		if checkErr != nil || generation != 0 {
			return "", "", fmt.Errorf("persisted admin credentials unavailable")
		}
		return user, password, nil // The file config is authoritative until the first save.
	}
	if err != nil {
		return "", "", err
	}
	var cfg struct {
		User     string `json:"admin_user"`
		Password string `json:"admin_pass"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil || strings.TrimSpace(cfg.Password) == "" {
		return "", "", fmt.Errorf("invalid persisted admin credentials")
	}
	return cfg.User, cfg.Password, nil
}

// NewRedisSessionBackend returns a durable session store, or nil when no Redis
// client is available. Callers keep the in-process store in that case.
func NewRedisSessionBackend(client *redis.Client, prefix string) SessionBackend {
	if client == nil {
		return nil
	}
	prefix = strings.TrimSpace(prefix)
	prefix = util.FirstNonEmptyUntrimmed(prefix, "orchids:")
	if !strings.HasSuffix(prefix, ":") {
		prefix += ":"
	}
	return &redisSessionBackend{client: client, prefix: prefix}
}

func (b *redisSessionBackend) key(token string) string {
	digest := sha256.Sum256([]byte(token))
	return b.prefix + "admin:sessions:" + hex.EncodeToString(digest[:])
}

func (b *redisSessionBackend) SaveSession(ctx context.Context, token string, expiry time.Time) error {
	if b == nil || b.client == nil {
		return nil
	}
	ttl := time.Until(expiry)
	if ttl <= 0 {
		// The session is already expired. Reporting success would let the caller
		// keep a token in its process-local store that the durable store never
		// accepted, leaving a session that is valid here and nowhere else.
		return fmt.Errorf("refusing to persist an already expired session")
	}
	return saveSessionScript.Run(ctx, b.client, []string{b.key(token), b.prefix + "admin:session_generation", b.prefix + "settings:config"}, ttl.Milliseconds(), "").Err()
}

var saveSessionScript = redis.NewScript(`
 local cfg = redis.call("GET", KEYS[3])
 if ARGV[2] ~= "" and cfg then
   local current = cjson.decode(cfg)
   if current.admin_pass ~= ARGV[2] then return redis.error_reply("admin credentials changed") end
 end
 local generation = redis.call("GET", KEYS[2]) or "1"
 redis.call("SET", KEYS[1], generation, "PX", ARGV[1])
 return 1
`)

func (b *redisSessionBackend) SaveSessionForPassword(ctx context.Context, token string, expiry time.Time, password string) error {
	ttl := time.Until(expiry)
	if ttl <= 0 {
		return fmt.Errorf("refusing to persist an already expired session")
	}
	return saveSessionScript.Run(ctx, b.client, []string{b.key(token), b.prefix + "admin:session_generation", b.prefix + "settings:config"}, ttl.Milliseconds(), password).Err()
}

var validateSessionScript = redis.NewScript(`
 local session = redis.call("GET", KEYS[1])
 local generation = redis.call("GET", KEYS[2]) or "1"
 if session and session == generation then return 1 end
 return 0
`)

func (b *redisSessionBackend) HasSession(ctx context.Context, token string) (bool, error) {
	if b == nil || b.client == nil {
		return false, nil
	}
	valid, err := validateSessionScript.Run(ctx, b.client, []string{b.key(token), b.prefix + "admin:session_generation"}).Int()
	return valid == 1, err
}

func (b *redisSessionBackend) DeleteSession(ctx context.Context, token string) {
	if b == nil || b.client == nil {
		return
	}
	_ = b.client.Del(ctx, b.key(token)).Err()
}
