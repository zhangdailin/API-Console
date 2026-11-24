package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// memoryResponseSweepThreshold bounds the map before an expired-entry sweep is
// attempted. Without it a long-lived process that stores many short-lived
// responses would only ever drop expired rows when they happen to be read again.
const memoryResponseSweepThreshold = 256

var ErrMemoryResponseStoreNotConfigured = errors.New("memory response store not configured")

// MemoryResponseStore is an in-process implementation of the stored-response
// contract, used when the gateway has no response backend to talk to.
//
// Redis stays the primary store: a stored response is resumed by whichever
// replica receives the next turn, so only a shared backend can serve a
// multi-replica deployment. This store exists for the single-process case — a
// gateway started without Redis, and tests that exercise the Responses bridge
// without standing one up — so store=true, previous_response_id and
// GET/DELETE /responses/{id} keep working instead of degrading to
// response_store_unavailable.
//
// Ownership is part of the key exactly as it is in Redis: a response id is
// readable only by the API-key fingerprint that created it. A miss and an
// expired row are both reported as ErrNoRows so callers keep the 404 they
// already produce for Redis.
type MemoryResponseStore struct {
	mu           sync.Mutex
	entries      map[string]StoredResponse
	ttl          time.Duration
	now          func() time.Time
	files        map[string]memoryResponseFile
	sweepFilesAt time.Time
}

type memoryResponseFile struct {
	payload   string
	expiresAt time.Time
}

// NewMemoryResponseStore returns a store whose records live for ttl. A
// non-positive ttl selects DefaultStoredResponseTTL.
func NewMemoryResponseStore(ttl time.Duration) *MemoryResponseStore {
	if ttl <= 0 {
		ttl = DefaultStoredResponseTTL
	}
	return &MemoryResponseStore{
		entries: make(map[string]StoredResponse),
		ttl:     ttl,
		now:     time.Now,
	}
}

var defaultMemoryResponseStore = NewMemoryResponseStore(0)

// DefaultMemoryResponseStore returns the process-wide in-process response
// store. Callers that have a real store must keep using it: this one is the
// fallback for a gateway with no response backend, not a cache in front of one.
func DefaultMemoryResponseStore() *MemoryResponseStore { return defaultMemoryResponseStore }

func memoryResponseKey(responseID, ownerHash string) string { return ownerHash + "\x00" + responseID }

func (s *MemoryResponseStore) timestamp() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

// sweepLocked drops expired entries. The caller must hold s.mu.
func (s *MemoryResponseStore) sweepLocked(now time.Time) {
	for key, entry := range s.entries {
		if !entry.ExpiresAt.IsZero() && !now.Before(entry.ExpiresAt) {
			delete(s.entries, key)
		}
	}
}

// SaveStoredResponse implements the response-store contract.
func (s *MemoryResponseStore) SaveStoredResponse(_ context.Context, response *StoredResponse, ttl time.Duration) error {
	if s == nil {
		return ErrMemoryResponseStoreNotConfigured
	}
	if ttl <= 0 {
		ttl = s.ttl
	}
	now := s.timestamp().UTC()
	stored, files, err := prepareStoredResponse(response, now, ttl)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= memoryResponseSweepThreshold {
		s.sweepLocked(now)
	}
	if !now.Before(s.sweepFilesAt) {
		for key, file := range s.files {
			if !file.expiresAt.After(now) {
				delete(s.files, key)
			}
		}
		s.sweepFilesAt = now.Add(time.Minute)
	}
	if len(files) > 0 && s.files == nil {
		s.files = make(map[string]memoryResponseFile)
	}
	for digest, payload := range files {
		key := memoryResponseKey(digest, stored.OwnerHash)
		file := s.files[key]
		if file.payload == "" {
			file.payload = payload
		}
		if file.expiresAt.Before(stored.ExpiresAt) {
			file.expiresAt = stored.ExpiresAt
		}
		s.files[key] = file
	}
	s.entries[memoryResponseKey(stored.ResponseID, stored.OwnerHash)] = stored
	return nil
}

func (s *MemoryResponseStore) GetStoredResponse(ctx context.Context, responseID, ownerHash string) (*StoredResponse, error) {
	stored, err := s.GetStoredResponseMetadata(ctx, responseID, ownerHash)
	if err != nil {
		return nil, err
	}
	if err := restoreResponseInputFiles(stored, func(digest string) (string, error) {
		s.mu.Lock()
		file := s.files[memoryResponseKey(digest, stored.OwnerHash)]
		s.mu.Unlock()
		if file.payload == "" {
			return "", errInvalidResponseInputFile
		}
		return file.payload, nil
	}); err != nil {
		return nil, err
	}
	return stored, nil
}

func (s *MemoryResponseStore) GetStoredResponseMetadata(_ context.Context, responseID, ownerHash string) (*StoredResponse, error) {
	if s == nil {
		return nil, ErrMemoryResponseStoreNotConfigured
	}
	responseID = strings.TrimSpace(responseID)
	ownerHash = strings.TrimSpace(ownerHash)
	if responseID == "" || ownerHash == "" {
		return nil, ErrNoRows
	}
	key := memoryResponseKey(responseID, ownerHash)

	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.entries[key]
	if !ok {
		return nil, ErrNoRows
	}
	now := s.timestamp().UTC()
	if !stored.ExpiresAt.IsZero() && !now.Before(stored.ExpiresAt) {
		delete(s.entries, key)
		return nil, ErrNoRows
	}
	return &stored, nil
}

func (s *MemoryResponseStore) UpdateStoredResponseBody(_ context.Context, previous *StoredResponse, body []byte) error {
	if s == nil {
		return ErrMemoryResponseStoreNotConfigured
	}
	if previous == nil {
		return ErrNoRows
	}
	key := memoryResponseKey(previous.ResponseID, previous.OwnerHash)
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.entries[key]
	now := s.timestamp().UTC()
	if !ok || !stored.ExpiresAt.After(now) {
		return ErrNoRows
	}
	if !stored.bodyMatches(previous) {
		return ErrStoredResponseChanged
	}
	stored.Body, stored.UpdatedAt = bytes.Clone(body), now
	s.entries[key] = stored
	return nil
}

// DeleteStoredResponse implements the response-store contract.
func (s *MemoryResponseStore) DeleteStoredResponse(_ context.Context, responseID, ownerHash string) error {
	if s == nil {
		return ErrMemoryResponseStoreNotConfigured
	}
	responseID = strings.TrimSpace(responseID)
	ownerHash = strings.TrimSpace(ownerHash)
	if responseID == "" || ownerHash == "" {
		return ErrNoRows
	}
	key := memoryResponseKey(responseID, ownerHash)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[key]; !ok {
		return ErrNoRows
	}
	delete(s.entries, key)
	return nil
}
