package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	DoubaoFileLimit       = 16 << 20
	DoubaoFileTTL         = time.Hour
	doubaoConversationTTL = 24 * time.Hour
	doubaoFilesPerOwner   = 8
)

var ErrDoubaoFileLimit = errors.New("too many active Doubao uploads")
var ErrDoubaoVideoCooldown = errors.New("Doubao video accounts are cooling down")

// SetDoubaoVideoCooldown records a quota rejection against the observed
// identity without rewriting account status, configuration, or credentials.
func (s *Store) SetDoubaoVideoCooldown(ctx context.Context, observed *Account, model string, until time.Time) error {
	if s == nil {
		return fmt.Errorf("Doubao quota store is unavailable")
	}
	rs, ok := s.accounts.(*redisStore)
	if !ok || observed == nil {
		return fmt.Errorf("Doubao quota store is unavailable")
	}
	return rs.updateAccountAtomic(ctx, observed.ID, func(current *Account) error {
		if current.AccountType != "doubao" || current.UserID != observed.UserID ||
			current.DoubaoDeviceID != observed.DoubaoDeviceID || current.DoubaoWebID != observed.DoubaoWebID {
			return fmt.Errorf("Doubao account identity changed during media generation")
		}
		current.ModelCooldowns = mergeModelCooldowns(nil, current.ModelCooldowns)
		current.ModelCooldownReasons = mergeModelCooldownReasons(nil, current.ModelCooldownReasons, nil, current.ModelCooldowns)
		RecordModelCooldownWithReason(current, model, until, ModelCooldownThrottled)
		return nil
	}, true)
}

// SetDoubaoVerificationPark exposes a real media challenge to the same
// administrator workflow used for chat. A newer session or verdict wins.
func (s *Store) SetDoubaoVerificationPark(ctx context.Context, observed *Account, message string, at, until time.Time) error {
	if s == nil || observed == nil {
		return fmt.Errorf("Doubao verification store is unavailable")
	}
	rs, ok := s.accounts.(*redisStore)
	if !ok {
		return fmt.Errorf("Doubao verification store is unavailable")
	}
	return rs.updateAccountAtomic(ctx, observed.ID, func(current *Account) error {
		if current.AccountType != "doubao" || !AccountAuthActive(current) ||
			DoubaoSessionVersion(current) != DoubaoSessionVersion(observed) ||
			current.LastAttempt.After(at) || current.QuotaResetAt.After(until) {
			return errAccountUnchanged
		}
		current.StatusCode, current.StatusMessage = "429", message
		current.LastAttempt, current.QuotaResetAt = at, until
		return nil
	}, true)
}

// DoubaoFile is a temporary, owner-scoped upload. Keeping bytes rather than a
// provider URI lets an eligible new turn attach it using its selected account.
type DoubaoFile struct {
	ID, Name  string
	Image     bool
	CreatedAt int64
	Data      []byte
}

type DoubaoFileMetadata struct {
	ID, Name  string
	Image     bool
	CreatedAt int64
	Size      int
}

func (file DoubaoFile) Metadata() DoubaoFileMetadata {
	return DoubaoFileMetadata{ID: file.ID, Name: file.Name, Image: file.Image, CreatedAt: file.CreatedAt, Size: len(file.Data)}
}

var saveDoubaoFile = redis.NewScript(`
	redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', ARGV[1])
	if redis.call('ZCARD', KEYS[2]) >= tonumber(ARGV[2]) then return 0 end
	if not redis.call('SET', KEYS[1], ARGV[3], 'NX', 'EX', ARGV[4]) then return -1 end
	redis.call('SET', KEYS[3], ARGV[6], 'EX', ARGV[4])
	redis.call('ZADD', KEYS[2], ARGV[5], KEYS[1])
	redis.call('EXPIRE', KEYS[2], ARGV[4])
	return 1
`)

var readDoubaoFileMetadata = redis.NewScript(`
	if redis.call('EXISTS', KEYS[1]) == 0 then return false end
	return redis.call('GET', KEYS[2])
`)

func (s *Store) SaveDoubaoFile(ctx context.Context, owner string, file DoubaoFile) error {
	rs, err := s.doubaoResourceStore(owner)
	if err != nil {
		return err
	}
	if file.ID == "" || len(file.Data) == 0 || len(file.Data) > DoubaoFileLimit {
		return fmt.Errorf("invalid Doubao upload")
	}
	raw, err := json.Marshal(file)
	if err != nil {
		return err
	}
	encoded, err := rs.credentials.encrypt(string(raw))
	if err != nil {
		return err
	}
	small, err := json.Marshal(file.Metadata())
	if err != nil {
		return err
	}
	metadata, err := rs.credentials.encrypt(string(small))
	if err != nil {
		return err
	}
	key := rs.digestKey("doubao:file:", owner, file.ID)
	index := rs.digestKey("doubao:file-index:", owner)
	now := time.Now()
	n, err := saveDoubaoFile.Run(ctx, rs.client, []string{key, index, key + ":meta"}, now.UnixMilli(), doubaoFilesPerOwner,
		encoded, int(DoubaoFileTTL.Seconds()), now.Add(DoubaoFileTTL).UnixMilli(), metadata).Int()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDoubaoFileLimit
	}
	if n != 1 {
		return fmt.Errorf("Doubao upload already exists")
	}
	return nil
}

func (s *Store) GetDoubaoFileMetadata(ctx context.Context, owner, id string) (DoubaoFileMetadata, error) {
	var metadata DoubaoFileMetadata
	rs, err := s.doubaoResourceStore(owner)
	if err != nil {
		return metadata, err
	}
	key := rs.digestKey("doubao:file:", owner, id)
	raw, err := readDoubaoFileMetadata.Run(ctx, rs.client, []string{key, key + ":meta"}).Text()
	if errors.Is(err, redis.Nil) {
		// Uploads made by an older binary still work until their short TTL ends.
		file, err := s.GetDoubaoFile(ctx, owner, id)
		if err != nil {
			return metadata, err
		}
		return file.Metadata(), nil
	}
	if err != nil {
		return metadata, err
	}
	plain, err := rs.credentials.decrypt(raw)
	if err != nil {
		return metadata, err
	}
	if err := json.Unmarshal([]byte(plain), &metadata); err != nil {
		return metadata, err
	}
	return metadata, nil
}

func (s *Store) GetDoubaoFile(ctx context.Context, owner, id string) (*DoubaoFile, error) {
	rs, err := s.doubaoResourceStore(owner)
	if err != nil {
		return nil, err
	}
	raw, err := rs.client.Get(ctx, rs.digestKey("doubao:file:", owner, id)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	plain, err := rs.credentials.decrypt(raw)
	if err != nil {
		return nil, err
	}
	var file DoubaoFile
	if err := json.Unmarshal([]byte(plain), &file); err != nil {
		return nil, err
	}
	return &file, nil
}

func (s *Store) DeleteDoubaoFile(ctx context.Context, owner, id string) error {
	rs, err := s.doubaoResourceStore(owner)
	if err != nil {
		return err
	}
	key := rs.digestKey("doubao:file:", owner, id)
	tx := rs.client.TxPipeline()
	deleted := tx.Del(ctx, key)
	tx.Del(ctx, key+":meta")
	tx.ZRem(ctx, rs.digestKey("doubao:file-index:", owner), key)
	if _, err := tx.Exec(ctx); err != nil {
		return err
	}
	if deleted.Val() == 0 {
		return ErrNoRows
	}
	return nil
}

func (s *Store) doubaoResourceStore(owner string) (*redisStore, error) {
	if s != nil && owner != "" {
		if rs, ok := s.accounts.(*redisStore); ok && rs.client != nil {
			return rs, nil
		}
	}
	return nil, fmt.Errorf("Doubao resource storage or owner is unavailable")
}

func (s *Store) DoubaoConversationAccount(ctx context.Context, owner, dialog string) (int64, error) {
	rs, err := s.doubaoResourceStore(owner)
	if err != nil {
		return 0, err
	}
	account, err := rs.client.Get(ctx, rs.digestKey("doubao:dialog:", owner, dialog)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return account, err
}

var bindDoubaoConversation = redis.NewScript(`
	local current = redis.call('GET', KEYS[1])
	if current then
		if current == ARGV[1] then redis.call('EXPIRE', KEYS[1], ARGV[2]) end
		return current
	end
	redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
	return ARGV[1]
`)

func (s *Store) BindDoubaoConversation(ctx context.Context, owner, dialog string, account int64) (int64, error) {
	rs, err := s.doubaoResourceStore(owner)
	if err != nil {
		return 0, err
	}
	if account <= 0 {
		return 0, fmt.Errorf("invalid Doubao conversation account")
	}
	result, err := bindDoubaoConversation.Run(ctx, rs.client, []string{rs.digestKey("doubao:dialog:", owner, dialog)},
		strconv.FormatInt(account, 10), int(doubaoConversationTTL.Seconds())).Text()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(result, 10, 64)
}
