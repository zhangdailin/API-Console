package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Grok Web media state: asynchronous video jobs and temporary media inputs.
// Both are owner-scoped and expire; media bytes live in the local media cache
// and these records only carry metadata.

// StoredVideoJob is the durable, owner-scoped state required to retrieve an
// asynchronous video result after the serving process restarts. Media bytes
// remain in the configured local cache; this record only stores metadata.
type StoredVideoJob struct {
	ID                string    `json:"id"`
	OwnerHash         string    `json:"owner_hash"`
	AccountID         int64     `json:"account_id,omitempty"`
	Provider          string    `json:"provider,omitempty"`
	Model             string    `json:"model"`
	Prompt            string    `json:"prompt,omitempty"`
	Seconds           int       `json:"seconds,omitempty"`
	Size              string    `json:"size,omitempty"`
	Quality           string    `json:"quality,omitempty"`
	Status            string    `json:"status"`
	Progress          int       `json:"progress"`
	VideoURL          string    `json:"video_url,omitempty"`
	ContentPath       string    `json:"content_path,omitempty"`
	UpstreamRequestID string    `json:"upstream_request_id,omitempty"`
	BuildFallback     bool      `json:"build_fallback,omitempty"`
	RemixedFromID     string    `json:"remixed_from_id,omitempty"`
	Operation         string    `json:"operation,omitempty"`
	StandardAPI       bool      `json:"standard_api,omitempty"`
	ErrorCode         string    `json:"error_code,omitempty"`
	ErrorMessage      string    `json:"error_message,omitempty"`
	CreatedAt         int64     `json:"created_at"`
	CompletedAt       int64     `json:"completed_at,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// StoredMediaInput points at an immutable temporary image or video accepted
// for later use through a standard API file_id.
type StoredMediaInput struct {
	ID          string    `json:"id"`
	OwnerHash   string    `json:"owner_hash"`
	Kind        string    `json:"kind"`
	MIMEType    string    `json:"mime_type"`
	ContentPath string    `json:"content_path"`
	SizeBytes   int64     `json:"size_bytes"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

var (
	refreshVideoJobLeaseScript = redis.NewScript(`
		if redis.call("GET", KEYS[1]) ~= ARGV[1] then return 0 end
		redis.call("PEXPIRE", KEYS[1], ARGV[2])
		return 1
	`)
	releaseVideoJobLeaseScript = redis.NewScript(`
		if redis.call("GET", KEYS[1]) ~= ARGV[1] then return 0 end
		return redis.call("DEL", KEYS[1])
	`)
)

func (s *redisStore) storedVideoJobKey(id, ownerHash string) string {
	return s.digestKey("videos:jobs:", strings.TrimSpace(ownerHash), strings.TrimSpace(id))
}

func (s *redisStore) storedVideoJobsIndexKey() string {
	return s.prefix + "videos:jobs:index"
}

func (s *redisStore) storedVideoJobLeaseKey(id, ownerHash string) string {
	return s.digestKey("videos:leases:", strings.TrimSpace(ownerHash), strings.TrimSpace(id))
}

func (s *redisStore) storedMediaInputKey(id, ownerHash string) string {
	return s.digestKey("media:inputs:", strings.TrimSpace(ownerHash), strings.TrimSpace(id))
}

func (s *redisStore) SaveStoredVideoJob(ctx context.Context, job *StoredVideoJob, ttl time.Duration) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if job == nil || strings.TrimSpace(job.ID) == "" || strings.TrimSpace(job.OwnerHash) == "" {
		return fmt.Errorf("video job id and owner are required")
	}
	if ttl <= 0 {
		return fmt.Errorf("video job ttl must be positive")
	}
	now := time.Now().UTC()
	stored := *job
	stored.ID = strings.TrimSpace(stored.ID)
	stored.OwnerHash = strings.TrimSpace(stored.OwnerHash)
	stored.UpdatedAt = now
	stored.ExpiresAt = now.Add(ttl)
	data, err := json.Marshal(&stored)
	if err != nil {
		return err
	}
	key := s.storedVideoJobKey(stored.ID, stored.OwnerHash)
	pipe := s.client.TxPipeline()
	pipe.Set(ctx, key, data, ttl)
	pipe.ZAdd(ctx, s.storedVideoJobsIndexKey(), redis.Z{Score: float64(stored.ExpiresAt.Unix()), Member: key})
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) GetStoredVideoJob(ctx context.Context, id, ownerHash string) (*StoredVideoJob, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	value, err := s.client.Get(ctx, s.storedVideoJobKey(id, ownerHash)).Bytes()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	var job StoredVideoJob
	if err := json.Unmarshal(value, &job); err != nil {
		return nil, err
	}
	if !job.ExpiresAt.IsZero() && !time.Now().UTC().Before(job.ExpiresAt) {
		key := s.storedVideoJobKey(id, ownerHash)
		pipe := s.client.TxPipeline()
		pipe.Del(ctx, key)
		pipe.ZRem(ctx, s.storedVideoJobsIndexKey(), key)
		_, _ = pipe.Exec(ctx)
		return nil, ErrNoRows
	}
	return &job, nil
}

func (s *redisStore) ListStoredVideoJobs(ctx context.Context) ([]*StoredVideoJob, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	now := time.Now().UTC()
	indexKey := s.storedVideoJobsIndexKey()
	if err := s.client.ZRemRangeByScore(ctx, indexKey, "-inf", strconv.FormatInt(now.Unix(), 10)).Err(); err != nil {
		return nil, err
	}
	keys, err := s.client.ZRangeArgs(ctx, redis.ZRangeArgs{Key: indexKey, Start: strconv.FormatInt(now.Unix()+1, 10), Stop: "+inf", ByScore: true}).Result()
	if err != nil || len(keys) == 0 {
		return nil, err
	}
	// Page each MGET so no single variadic command grows without bound. The
	// method still returns every live job (its existing contract).
	jobs := make([]*StoredVideoJob, 0, len(keys))
	stale := make([]any, 0)
	const mgetBatch = 256
	for start := 0; start < len(keys); start += mgetBatch {
		end := min(start+mgetBatch, len(keys))
		values, err := s.client.MGet(ctx, keys[start:end]...).Result()
		if err != nil {
			return nil, err
		}
		for index, value := range values {
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				stale = append(stale, keys[start+index])
				continue
			}
			var job StoredVideoJob
			if json.Unmarshal([]byte(text), &job) != nil || (!job.ExpiresAt.IsZero() && !now.Before(job.ExpiresAt)) {
				stale = append(stale, keys[start+index])
				continue
			}
			jobs = append(jobs, &job)
		}
	}
	if len(stale) > 0 {
		_ = s.client.ZRem(ctx, indexKey, stale...).Err()
	}
	return jobs, nil
}

func (s *redisStore) DeleteStoredVideoJob(ctx context.Context, id, ownerHash string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	id, ownerHash = strings.TrimSpace(id), strings.TrimSpace(ownerHash)
	if id == "" || ownerHash == "" {
		return fmt.Errorf("video job id and owner are required")
	}
	key := s.storedVideoJobKey(id, ownerHash)
	pipe := s.client.TxPipeline()
	pipe.Del(ctx, key)
	pipe.ZRem(ctx, s.storedVideoJobsIndexKey(), key)
	_, err := pipe.Exec(ctx)
	return err
}

func validateVideoJobLeaseArgs(id, ownerHash, holder string, ttl time.Duration) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(ownerHash) == "" || strings.TrimSpace(holder) == "" {
		return fmt.Errorf("video job id, owner, and lease holder are required")
	}
	if ttl <= 0 {
		return fmt.Errorf("video job lease ttl must be positive")
	}
	return nil
}

func (s *redisStore) AcquireVideoJobLease(ctx context.Context, id, ownerHash, holder string, ttl time.Duration) (bool, error) {
	if s == nil || s.client == nil {
		return false, fmt.Errorf("redis store not configured")
	}
	if err := validateVideoJobLeaseArgs(id, ownerHash, holder, ttl); err != nil {
		return false, err
	}
	return s.client.SetNX(ctx, s.storedVideoJobLeaseKey(id, ownerHash), strings.TrimSpace(holder), ttl).Result()
}

func (s *redisStore) RefreshVideoJobLease(ctx context.Context, id, ownerHash, holder string, ttl time.Duration) (bool, error) {
	if s == nil || s.client == nil {
		return false, fmt.Errorf("redis store not configured")
	}
	if err := validateVideoJobLeaseArgs(id, ownerHash, holder, ttl); err != nil {
		return false, err
	}
	value, err := refreshVideoJobLeaseScript.Run(ctx, s.client, []string{s.storedVideoJobLeaseKey(id, ownerHash)}, strings.TrimSpace(holder), ttl.Milliseconds()).Int64()
	return value == 1, err
}

func (s *redisStore) ReleaseVideoJobLease(ctx context.Context, id, ownerHash, holder string) (bool, error) {
	if s == nil || s.client == nil {
		return false, fmt.Errorf("redis store not configured")
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(ownerHash) == "" || strings.TrimSpace(holder) == "" {
		return false, fmt.Errorf("video job id, owner, and lease holder are required")
	}
	value, err := releaseVideoJobLeaseScript.Run(ctx, s.client, []string{s.storedVideoJobLeaseKey(id, ownerHash)}, strings.TrimSpace(holder)).Int64()
	return value == 1, err
}

func (s *redisStore) SaveStoredMediaInput(ctx context.Context, input *StoredMediaInput, ttl time.Duration) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if input == nil || strings.TrimSpace(input.ID) == "" || strings.TrimSpace(input.OwnerHash) == "" {
		return fmt.Errorf("media input id and owner are required")
	}
	if ttl <= 0 {
		return fmt.Errorf("media input ttl must be positive")
	}
	now := time.Now().UTC()
	stored := *input
	stored.ID = strings.TrimSpace(stored.ID)
	stored.OwnerHash = strings.TrimSpace(stored.OwnerHash)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = now
	}
	stored.ExpiresAt = now.Add(ttl)
	data, err := json.Marshal(&stored)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.storedMediaInputKey(stored.ID, stored.OwnerHash), data, ttl).Err()
}

func (s *redisStore) GetStoredMediaInput(ctx context.Context, id, ownerHash string) (*StoredMediaInput, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	key := s.storedMediaInputKey(id, ownerHash)
	return getExpiringRedisJSON[StoredMediaInput](ctx, s, key, func(input *StoredMediaInput) time.Time {
		return input.ExpiresAt
	})
}

func (s *redisStore) DeleteStoredMediaInput(ctx context.Context, id, ownerHash string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	deleted, err := s.client.Del(ctx, s.storedMediaInputKey(id, ownerHash)).Result()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrNoRows
	}
	return nil
}

func (s *Store) SaveStoredVideoJob(ctx context.Context, job *StoredVideoJob, ttl time.Duration) error {
	if s == nil || s.webMedia == nil {
		return fmt.Errorf("video job store not configured")
	}
	return s.webMedia.SaveStoredVideoJob(ctx, job, ttl)
}

func (s *Store) GetStoredVideoJob(ctx context.Context, id, ownerHash string) (*StoredVideoJob, error) {
	if s == nil || s.webMedia == nil {
		return nil, fmt.Errorf("video job store not configured")
	}
	return s.webMedia.GetStoredVideoJob(ctx, id, ownerHash)
}

func (s *Store) ListStoredVideoJobs(ctx context.Context) ([]*StoredVideoJob, error) {
	if s == nil || s.webMedia == nil {
		return nil, fmt.Errorf("video job store not configured")
	}
	return s.webMedia.ListStoredVideoJobs(ctx)
}

func (s *Store) DeleteStoredVideoJob(ctx context.Context, id, ownerHash string) error {
	if s == nil || s.webMedia == nil {
		return fmt.Errorf("video job store not configured")
	}
	return s.webMedia.DeleteStoredVideoJob(ctx, id, ownerHash)
}

func (s *Store) AcquireVideoJobLease(ctx context.Context, id, ownerHash, holder string, ttl time.Duration) (bool, error) {
	if s == nil || s.webMedia == nil {
		return false, fmt.Errorf("video job store not configured")
	}
	return s.webMedia.AcquireVideoJobLease(ctx, id, ownerHash, holder, ttl)
}

func (s *Store) RefreshVideoJobLease(ctx context.Context, id, ownerHash, holder string, ttl time.Duration) (bool, error) {
	if s == nil || s.webMedia == nil {
		return false, fmt.Errorf("video job store not configured")
	}
	return s.webMedia.RefreshVideoJobLease(ctx, id, ownerHash, holder, ttl)
}

func (s *Store) ReleaseVideoJobLease(ctx context.Context, id, ownerHash, holder string) (bool, error) {
	if s == nil || s.webMedia == nil {
		return false, fmt.Errorf("video job store not configured")
	}
	return s.webMedia.ReleaseVideoJobLease(ctx, id, ownerHash, holder)
}

func (s *Store) SaveStoredMediaInput(ctx context.Context, input *StoredMediaInput, ttl time.Duration) error {
	if s == nil || s.webMedia == nil {
		return fmt.Errorf("media input store not configured")
	}
	return s.webMedia.SaveStoredMediaInput(ctx, input, ttl)
}

func (s *Store) GetStoredMediaInput(ctx context.Context, id, ownerHash string) (*StoredMediaInput, error) {
	if s == nil || s.webMedia == nil {
		return nil, fmt.Errorf("media input store not configured")
	}
	return s.webMedia.GetStoredMediaInput(ctx, id, ownerHash)
}

func (s *Store) DeleteStoredMediaInput(ctx context.Context, id, ownerHash string) error {
	if s == nil || s.webMedia == nil {
		return fmt.Errorf("media input store not configured")
	}
	return s.webMedia.DeleteStoredMediaInput(ctx, id, ownerHash)
}
