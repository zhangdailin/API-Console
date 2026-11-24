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
	if s == nil || s.client == nil || observed == nil {
		return fmt.Errorf("Doubao quota store is unavailable")
	}
	return s.updateAccountAtomic(ctx, observed.ID, func(current *Account) error {
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
	if s == nil || s.client == nil || observed == nil {
		return fmt.Errorf("Doubao verification store is unavailable")
	}
	return s.updateAccountAtomic(ctx, observed.ID, func(current *Account) error {
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
	if err := s.requireDoubaoResourceOwner(owner); err != nil {
		return err
	}
	if file.ID == "" || len(file.Data) == 0 || len(file.Data) > DoubaoFileLimit {
		return fmt.Errorf("invalid Doubao upload")
	}
	raw, err := json.Marshal(file)
	if err != nil {
		return err
	}
	encoded, err := s.credentials.encrypt(string(raw))
	if err != nil {
		return err
	}
	small, err := json.Marshal(file.Metadata())
	if err != nil {
		return err
	}
	metadata, err := s.credentials.encrypt(string(small))
	if err != nil {
		return err
	}
	key := s.digestKey("doubao:file:", owner, file.ID)
	index := s.digestKey("doubao:file-index:", owner)
	now := time.Now()
	n, err := saveDoubaoFile.Run(ctx, s.client, []string{key, index, key + ":meta"}, now.UnixMilli(), doubaoFilesPerOwner,
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
	if err := s.requireDoubaoResourceOwner(owner); err != nil {
		return metadata, err
	}
	key := s.digestKey("doubao:file:", owner, id)
	raw, err := readDoubaoFileMetadata.Run(ctx, s.client, []string{key, key + ":meta"}).Text()
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
	plain, err := s.credentials.decrypt(raw)
	if err != nil {
		return metadata, err
	}
	if err := json.Unmarshal([]byte(plain), &metadata); err != nil {
		return metadata, err
	}
	return metadata, nil
}

func (s *Store) GetDoubaoFile(ctx context.Context, owner, id string) (*DoubaoFile, error) {
	if err := s.requireDoubaoResourceOwner(owner); err != nil {
		return nil, err
	}
	raw, err := s.client.Get(ctx, s.digestKey("doubao:file:", owner, id)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.credentials.decrypt(raw)
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
	if err := s.requireDoubaoResourceOwner(owner); err != nil {
		return err
	}
	key := s.digestKey("doubao:file:", owner, id)
	tx := s.client.TxPipeline()
	deleted := tx.Del(ctx, key)
	tx.Del(ctx, key+":meta")
	tx.ZRem(ctx, s.digestKey("doubao:file-index:", owner), key)
	if _, err := tx.Exec(ctx); err != nil {
		return err
	}
	if deleted.Val() == 0 {
		return ErrNoRows
	}
	return nil
}

func (s *Store) requireDoubaoResourceOwner(owner string) error {
	if owner == "" || s == nil || s.client == nil {
		return fmt.Errorf("Doubao resource storage or owner is unavailable")
	}
	return nil
}

func (s *Store) DoubaoConversationAccount(ctx context.Context, owner, dialog string) (int64, error) {
	if err := s.requireDoubaoResourceOwner(owner); err != nil {
		return 0, err
	}
	account, err := s.client.Get(ctx, s.digestKey("doubao:dialog:", owner, dialog)).Int64()
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
	if err := s.requireDoubaoResourceOwner(owner); err != nil {
		return 0, err
	}
	if account <= 0 {
		return 0, fmt.Errorf("invalid Doubao conversation account")
	}
	result, err := bindDoubaoConversation.Run(ctx, s.client, []string{s.digestKey("doubao:dialog:", owner, dialog)},
		strconv.FormatInt(account, 10), int(doubaoConversationTTL.Seconds())).Text()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(result, 10, 64)
}
