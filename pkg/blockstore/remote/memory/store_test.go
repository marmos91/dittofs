package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/blockstoretest"
)

// TestStore_BlockStoreConformance runs the unified
// BlockStoreConformance suite against the in-memory remote backend.
// Per the remote backends implement only BlockStore (no
// BlockStoreAppend); only BlockStoreConformance runs here.
//
// The inline TestStore_* tests below remain in place as a fine-grained
// per-method baseline (data-isolation defensive copies, closed-store
// rejection paths, ReadBlockVerified mismatch case) — they exercise
// backend-specific behaviors that are not part of the unified contract.
//
// -07 lands the missing Has() method on the remote-memory
// *Store; until then the factory return type does not type-check.
func TestStore_BlockStoreConformance(t *testing.T) {
	factory := func(t *testing.T) (blockstore.BlockStore, func()) {
		t.Helper()
		s := New()
		cleanup := func() { _ = s.Close() }
		return s, cleanup
	}
	blockstoretest.BlockStoreConformance(t, factory)
}

// hashOf returns the BLAKE3-256 hash of data as a blockstore.ContentHash.
func hashOf(t *testing.T, data []byte) blockstore.ContentHash {
	t.Helper()
	sum := blake3.Sum256(data)
	var h blockstore.ContentHash
	copy(h[:], sum[:])
	return h
}

func TestStore_PutAndGet(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("hello world")
	hash := hashOf(t, data)

	if err := s.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	read, err := s.Get(ctx, hash)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if string(read) != string(data) {
		t.Errorf("Get returned %q, want %q", read, data)
	}
}

func TestStore_GetNotFound(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	hash := hashOf(t, []byte("nonexistent"))
	_, err := s.Get(ctx, hash)
	if !errors.Is(err, blockstore.ErrChunkNotFound) {
		t.Errorf("Get returned error %v, want %v", err, blockstore.ErrChunkNotFound)
	}
}

func TestStore_GetRange(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("hello world")
	hash := hashOf(t, data)

	if err := s.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	read, err := s.GetRange(ctx, hash, 0, 5)
	if err != nil {
		t.Fatalf("GetRange failed: %v", err)
	}
	if string(read) != "hello" {
		t.Errorf("GetRange returned %q, want %q", read, "hello")
	}

	read, err = s.GetRange(ctx, hash, 6, 5)
	if err != nil {
		t.Fatalf("GetRange failed: %v", err)
	}
	if string(read) != "world" {
		t.Errorf("GetRange returned %q, want %q", read, "world")
	}

	// Read range that exceeds length (should truncate)
	read, err = s.GetRange(ctx, hash, 6, 100)
	if err != nil {
		t.Fatalf("GetRange failed: %v", err)
	}
	if string(read) != "world" {
		t.Errorf("GetRange returned %q, want %q", read, "world")
	}
}

func TestStore_Delete(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("hello world")
	hash := hashOf(t, data)

	if err := s.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	if err := s.Delete(ctx, hash); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	_, err := s.Get(ctx, hash)
	if !errors.Is(err, blockstore.ErrChunkNotFound) {
		t.Errorf("Get after delete returned %v, want %v", err, blockstore.ErrChunkNotFound)
	}

	// Delete on absent hash is idempotent.
	if err := s.Delete(ctx, hash); err != nil {
		t.Errorf("Delete on absent hash returned %v, want nil (idempotent)", err)
	}
}

func TestStore_Head(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("head fixture")
	hash := hashOf(t, data)

	if err := s.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	meta, err := s.Head(ctx, hash)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if meta.Size != int64(len(data)) {
		t.Errorf("Head Size = %d, want %d", meta.Size, len(data))
	}
	if meta.LastModified.IsZero() {
		t.Error("Head LastModified is zero — WR-4-02 contract violation")
	}

	// Missing hash
	missing := hashOf(t, []byte("does-not-exist"))
	if _, err := s.Head(ctx, missing); !errors.Is(err, blockstore.ErrChunkNotFound) {
		t.Errorf("Head on missing hash = %v, want %v", err, blockstore.ErrChunkNotFound)
	}
}

// TestStore_Walk asserts the Walk contract: every stored
// CAS object is visited; the callback receives a non-zero Meta; ordering
// is unspecified so we collect into a set.
func TestStore_Walk(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	want := map[blockstore.ContentHash][]byte{}
	for i := 0; i < 5; i++ {
		data := []byte(fmt.Sprintf("walk fixture %d", i))
		h := hashOf(t, data)
		want[h] = data
		if err := s.Put(ctx, h, data); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	seen := map[blockstore.ContentHash]bool{}
	err := s.Walk(ctx, func(hash blockstore.ContentHash, meta blockstore.Meta) error {
		seen[hash] = true
		exp, ok := want[hash]
		if !ok {
			return fmt.Errorf("Walk surfaced unknown hash %s", hash)
		}
		if meta.Size != int64(len(exp)) {
			return fmt.Errorf("Walk meta.Size = %d, want %d", meta.Size, len(exp))
		}
		if meta.LastModified.IsZero() {
			return fmt.Errorf("Walk meta.LastModified is zero")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for h := range want {
		if !seen[h] {
			t.Errorf("Walk did not surface hash %s", h)
		}
	}
}

// TestStore_Walk_ErrStopWalk pins the early-exit contract
// returning blockstore.ErrStopWalk from the callback exits cleanly with
// nil from Walk.
func TestStore_Walk_ErrStopWalk(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	for i := 0; i < 3; i++ {
		data := []byte(fmt.Sprintf("stopwalk %d", i))
		if err := s.Put(ctx, hashOf(t, data), data); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	seen := 0
	err := s.Walk(ctx, func(_ blockstore.ContentHash, _ blockstore.Meta) error {
		seen++
		if seen == 1 {
			return blockstore.ErrStopWalk
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk should return nil on ErrStopWalk, got %v", err)
	}
	if seen != 1 {
		t.Fatalf("Walk should stop after first ErrStopWalk, saw %d objects", seen)
	}
}

// TestStore_Walk_CallbackErrorWrapped pins the contract that
// non-ErrStopWalk callback errors are wrapped as "walk halted at %s: %w".
func TestStore_Walk_CallbackErrorWrapped(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("wrap me")
	if err := s.Put(ctx, hashOf(t, data), data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	sentinel := errors.New("callback boom")
	err := s.Walk(ctx, func(_ blockstore.ContentHash, _ blockstore.Meta) error {
		return sentinel
	})
	if err == nil {
		t.Fatal("Walk should propagate callback error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Walk err = %v, want wrapped %v", err, sentinel)
	}
}

// TestStore_ReadBlockVerified covers the happy path and the
// body-mismatch failure mode.
func TestStore_ReadBlockVerified(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("verified read")
	hash := hashOf(t, data)
	if err := s.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.ReadBlockVerified(ctx, hash, hash)
	if err != nil {
		t.Fatalf("ReadBlockVerified happy path: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("ReadBlockVerified bytes mismatch: got %q, want %q", got, data)
	}

	// Mismatched expected => ErrCASContentMismatch
	wrong := hash
	wrong[0] ^= 0xFF
	if _, err := s.ReadBlockVerified(ctx, hash, wrong); !errors.Is(err, blockstore.ErrCASContentMismatch) {
		t.Fatalf("ReadBlockVerified mismatch err = %v, want wrapped ErrCASContentMismatch", err)
	}

	// Not found
	missing := hashOf(t, []byte("missing"))
	if _, err := s.ReadBlockVerified(ctx, missing, missing); !errors.Is(err, blockstore.ErrChunkNotFound) {
		t.Fatalf("ReadBlockVerified on missing hash = %v, want wrapped ErrChunkNotFound", err)
	}
}

func TestStore_ClosedOperations(t *testing.T) {
	ctx := context.Background()
	s := New()

	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	hash := hashOf(t, []byte("data"))

	if _, err := s.Get(ctx, hash); !errors.Is(err, blockstore.ErrStoreClosed) {
		t.Errorf("Get on closed store returned %v, want %v", err, blockstore.ErrStoreClosed)
	}

	if err := s.Put(ctx, hash, []byte("data")); !errors.Is(err, blockstore.ErrStoreClosed) {
		t.Errorf("Put on closed store returned %v, want %v", err, blockstore.ErrStoreClosed)
	}

	if err := s.Delete(ctx, hash); !errors.Is(err, blockstore.ErrStoreClosed) {
		t.Errorf("Delete on closed store returned %v, want %v", err, blockstore.ErrStoreClosed)
	}

	if err := s.Walk(ctx, func(_ blockstore.ContentHash, _ blockstore.Meta) error { return nil }); !errors.Is(err, blockstore.ErrStoreClosed) {
		t.Errorf("Walk on closed store returned %v, want %v", err, blockstore.ErrStoreClosed)
	}
}

func TestStore_DataIsolation(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("hello world")
	hash := hashOf(t, data)

	if err := s.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Modify original data
	data[0] = 'X'

	read, err := s.Get(ctx, hash)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if read[0] != 'h' {
		t.Errorf("Put did not copy data: got %c, want 'h'", read[0])
	}

	// Modify read data
	read[0] = 'Y'

	read2, err := s.Get(ctx, hash)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if read2[0] != 'h' {
		t.Errorf("Get did not copy data: got %c, want 'h'", read2[0])
	}
}

func TestStore_BlockCount(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	if s.BlockCount() != 0 {
		t.Errorf("BlockCount on empty store returned %d, want 0", s.BlockCount())
	}

	if err := s.Put(ctx, hashOf(t, []byte("data1")), []byte("data1")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if err := s.Put(ctx, hashOf(t, []byte("data2")), []byte("data2")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	if s.BlockCount() != 2 {
		t.Errorf("BlockCount returned %d, want 2", s.BlockCount())
	}
}

func TestStore_TotalSize(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	if s.TotalSize() != 0 {
		t.Errorf("TotalSize on empty store returned %d, want 0", s.TotalSize())
	}

	if err := s.Put(ctx, hashOf(t, []byte("hello")), []byte("hello")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if err := s.Put(ctx, hashOf(t, []byte("world")), []byte("world")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	if s.TotalSize() != 10 {
		t.Errorf("TotalSize returned %d, want 10", s.TotalSize())
	}
}
