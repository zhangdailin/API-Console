package store

import (
	"context"
	"orchids-api/internal/testutil"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newBatchReadStore returns a store over an in-process Redis. The account read
// path is one MGET, so it needs no live server and no credential cipher: the
// seeded rows below carry plaintext markers only.
func newBatchReadStore(t *testing.T) *Store {
	t.Helper()
	mini := miniredis.RunT(t)
	s := &Store{
		client: redis.NewClient(&redis.Options{Addr: mini.Addr()}),
		prefix: "test:",
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestGetAccountsByIDsReadsEveryRowInOneBatch pins the batch read's contract:
// every requested row is decoded, in the requested order.
func TestGetAccountsByIDsReadsEveryRowInOneBatch(t *testing.T) {
	s := newBatchReadStore(t)
	ctx := context.Background()

	for id, body := range map[int64]string{
		1: `{"id":1,"name":"test1","enabled":true}`,
		2: `{"id":2,"name":"test2","enabled":true}`,
		3: `{"id":3,"name":"test3","enabled":false}`,
	} {
		err := s.client.Set(ctx, s.accountsKey(id), body, 0).Err()
		testutil.CheckNoError(t, err)
	}

	accounts, err := s.getAccountsByIDs(ctx, []string{"1", "2", "3"}, false)
	testutil.NoError(t, err, "getAccountsByIDs() error = %v")
	testutil.Equal(t, len(accounts), 3)
	for i, want := range []string{"test1", "test2", "test3"} {
		testutil.Equal(t, accounts[i].Name, want)
	}
}

// TestGetAccountsByIDsSkipsMissingAndDisabledRows covers the two rows the result
// must omit: an id with no stored row and a row the caller filtered out.
func TestGetAccountsByIDsSkipsMissingAndDisabledRows(t *testing.T) {
	s := newBatchReadStore(t)
	ctx := context.Background()

	testutil.NoError(t, s.client.Set(ctx, s.accountsKey(1), `{"id":1,"name":"enabled","enabled":true}`, 0).Err())
	testutil.NoError(t, s.client.Set(ctx, s.accountsKey(3), `{"id":3,"name":"disabled","enabled":false}`, 0).Err())

	// id 2 has no row at all.
	accounts, err := s.getAccountsByIDs(ctx, []string{"1", "2", "3"}, false)
	testutil.NoError(t, err, "getAccountsByIDs() error = %v")
	testutil.Equal(t, len(accounts), 2)

	enabled, err := s.getAccountsByIDs(ctx, []string{"1", "3"}, true)
	testutil.NoError(t, err, "getAccountsByIDs(onlyEnabled) error = %v")
	testutil.Equal(t, len(enabled), 1)
	testutil.Equal(t, enabled[0].ID, 1)
}

// TestGetAccountsByIDsEmptySelectionDoesNotReachRedis proves the empty and
// unparseable selections short-circuit instead of issuing a batch of zero keys.
func TestGetAccountsByIDsEmptySelectionDoesNotReachRedis(t *testing.T) {
	s := &Store{
		client: redis.NewClient(&redis.Options{Addr: "localhost:9999"}),
		prefix: "test:",
	}
	defer s.Close()
	ctx := context.Background()

	for _, ids := range [][]string{nil, {}, {"not-an-id"}} {
		accounts, err := s.getAccountsByIDs(ctx, ids, false)
		testutil.Falsef(t, err != nil, "getAccountsByIDs(%v) error = %v, want no round trip", ids, err)
		testutil.Falsef(t, accounts != nil, "getAccountsByIDs(%v) = %#v, want nil", ids, accounts)
	}
}

// TestGetAccountsByIDsReportsTransportFailure keeps the failure contract: an
// unreachable Redis is reported, never silently read as an empty pool.
func TestGetAccountsByIDsReportsTransportFailure(t *testing.T) {
	s := &Store{
		client: redis.NewClient(&redis.Options{Addr: "localhost:9999"}),
		prefix: "test:",
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := s.getAccountsByIDs(ctx, []string{"1"}, false)
	testutil.Error(t, err)
}
