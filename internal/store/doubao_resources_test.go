package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestDoubaoFileMetadataStaysSmallAndUsesTheSameLifetime(t *testing.T) {
	s, _ := newDoubaoIdentityTestStore(t)
	rs := s
	ctx := context.Background()
	owner, id := "fixture-owner", "fixture-file"
	file := DoubaoFile{ID: id, Name: "notes.txt", CreatedAt: 123, Data: bytes.Repeat([]byte("x"), 16384)}
	if err := s.SaveDoubaoFile(ctx, owner, file); err != nil {
		t.Fatal(err)
	}
	key := rs.digestKey("doubao:file:", owner, id)
	if n := rs.client.StrLen(ctx, key+":meta").Val(); n == 0 || n > 1024 {
		t.Fatalf("unexpected metadata payload length: %d", n)
	}
	if ttl := rs.client.TTL(ctx, key+":meta").Val(); ttl != rs.client.TTL(ctx, key).Val() {
		t.Fatal("metadata and body expire at different times")
	}
	if _, err := s.GetDoubaoFileMetadata(ctx, "other-owner", id); !errors.Is(err, ErrNoRows) {
		t.Fatalf("foreign lookup: %v", err)
	}
	// A metadata lookup uses the sidecar even if loading the full body would fail.
	body, err := rs.client.Get(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.client.Set(ctx, key, "unreadable fixture", DoubaoFileTTL).Err(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetDoubaoFileMetadata(ctx, owner, id); err != nil || got != file.Metadata() {
		t.Fatalf("metadata: %+v, %v", got, err)
	}
	if err := rs.client.Set(ctx, key, body, DoubaoFileTTL).Err(); err != nil {
		t.Fatal(err)
	}
	// Older versions write a body without a sidecar.
	if err := rs.client.Del(ctx, key+":meta").Err(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetDoubaoFileMetadata(ctx, owner, id); err != nil || got != file.Metadata() {
		t.Fatalf("legacy metadata: %+v, %v", got, err)
	}
	if err := s.DeleteDoubaoFile(ctx, owner, id); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDoubaoFile(ctx, owner, file); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDoubaoFile(ctx, owner, id); err != nil {
		t.Fatal(err)
	}
	if n := rs.client.Exists(ctx, key, key+":meta").Val(); n != 0 {
		t.Fatalf("%d file records remain", n)
	}
}
