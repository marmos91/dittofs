package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

func TestStore_WriteAndRead(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	blockKey := "share1/content123/block-0"
	data := []byte("hello world")

	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	read, err := s.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if string(read) != string(data) {
		t.Errorf("ReadBlock returned %q, want %q", read, data)
	}
}

func TestStore_ReadBlockNotFound(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	_, err := s.ReadBlock(ctx, "nonexistent")
	if !errors.Is(err, blockstore.ErrBlockNotFound) {
		t.Errorf("ReadBlock returned error %v, want %v", err, blockstore.ErrBlockNotFound)
	}
}

func TestStore_ReadBlockRange(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	blockKey := "share1/content123/block-0"
	data := []byte("hello world")

	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	read, err := s.ReadBlockRange(ctx, blockKey, 0, 5)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}
	if string(read) != "hello" {
		t.Errorf("ReadBlockRange returned %q, want %q", read, "hello")
	}

	read, err = s.ReadBlockRange(ctx, blockKey, 6, 5)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}
	if string(read) != "world" {
		t.Errorf("ReadBlockRange returned %q, want %q", read, "world")
	}

	// Read range that exceeds length (should truncate)
	read, err = s.ReadBlockRange(ctx, blockKey, 6, 100)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}
	if string(read) != "world" {
		t.Errorf("ReadBlockRange returned %q, want %q", read, "world")
	}
}

func TestStore_DeleteBlock(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	blockKey := "share1/content123/block-0"
	data := []byte("hello world")

	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	if err := s.DeleteBlock(ctx, blockKey); err != nil {
		t.Fatalf("DeleteBlock failed: %v", err)
	}

	_, err := s.ReadBlock(ctx, blockKey)
	if err != blockstore.ErrBlockNotFound {
		t.Errorf("ReadBlock after delete returned error %v, want %v", err, blockstore.ErrBlockNotFound)
	}
}

func TestStore_DeleteByPrefix(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	blocks := map[string][]byte{
		"share1/content123/block-0": []byte("data0"),
		"share1/content123/block-1": []byte("data1"),
		"share1/content123/block-2": []byte("data2"),
		"share2/content456/block-0": []byte("data3"),
	}

	for key, data := range blocks {
		if err := s.WriteBlock(ctx, key, data); err != nil {
			t.Fatalf("WriteBlock(%s) failed: %v", key, err)
		}
	}

	if err := s.DeleteByPrefix(ctx, "share1/content123/"); err != nil {
		t.Fatalf("DeleteByPrefix failed: %v", err)
	}

	for key := range blocks {
		_, err := s.ReadBlock(ctx, key)
		if key[:17] == "share1/content123" {
			if !errors.Is(err, blockstore.ErrBlockNotFound) {
				t.Errorf("ReadBlock(%s) after delete returned error %v, want %v", key, err, blockstore.ErrBlockNotFound)
			}
		} else {
			if err != nil {
				t.Errorf("ReadBlock(%s) after delete returned unexpected error: %v", key, err)
			}
		}
	}
}

func TestStore_ListByPrefix(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	blocks := map[string][]byte{
		"share1/content123/block-0": []byte("data0"),
		"share1/content123/block-1": []byte("data1"),
		"share1/content123/block-2": []byte("data2"),
		"share2/content456/block-0": []byte("data3"),
	}

	for key, data := range blocks {
		if err := s.WriteBlock(ctx, key, data); err != nil {
			t.Fatalf("WriteBlock(%s) failed: %v", key, err)
		}
	}

	keys, err := s.ListByPrefix(ctx, "share1/content123/")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}
	if len(keys) != 3 {
		t.Errorf("ListByPrefix returned %d keys, want 3", len(keys))
	}

	keys, err = s.ListByPrefix(ctx, "")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}
	if len(keys) != 4 {
		t.Errorf("ListByPrefix returned %d keys, want 4", len(keys))
	}
}

func TestStore_ClosedOperations(t *testing.T) {
	ctx := context.Background()
	s := New()

	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if _, err := s.ReadBlock(ctx, "key"); !errors.Is(err, blockstore.ErrStoreClosed) {
		t.Errorf("ReadBlock on closed store returned %v, want %v", err, blockstore.ErrStoreClosed)
	}

	if err := s.WriteBlock(ctx, "key", []byte("data")); !errors.Is(err, blockstore.ErrStoreClosed) {
		t.Errorf("WriteBlock on closed store returned %v, want %v", err, blockstore.ErrStoreClosed)
	}

	if err := s.DeleteBlock(ctx, "key"); !errors.Is(err, blockstore.ErrStoreClosed) {
		t.Errorf("DeleteBlock on closed store returned %v, want %v", err, blockstore.ErrStoreClosed)
	}

	if _, err := s.ListByPrefix(ctx, ""); !errors.Is(err, blockstore.ErrStoreClosed) {
		t.Errorf("ListByPrefix on closed store returned %v, want %v", err, blockstore.ErrStoreClosed)
	}
}

func TestStore_DataIsolation(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	blockKey := "share1/content123/block-0"
	data := []byte("hello world")

	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Modify original data
	data[0] = 'X'

	read, err := s.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if read[0] != 'h' {
		t.Errorf("WriteBlock did not copy data: got %c, want 'h'", read[0])
	}

	// Modify read data
	read[0] = 'Y'

	read2, err := s.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if read2[0] != 'h' {
		t.Errorf("ReadBlock did not copy data: got %c, want 'h'", read2[0])
	}
}

func TestStore_BlockCount(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	if s.BlockCount() != 0 {
		t.Errorf("BlockCount on empty store returned %d, want 0", s.BlockCount())
	}

	if err := s.WriteBlock(ctx, "key1", []byte("data1")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}
	if err := s.WriteBlock(ctx, "key2", []byte("data2")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
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

	if err := s.WriteBlock(ctx, "key1", []byte("hello")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}
	if err := s.WriteBlock(ctx, "key2", []byte("world")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	if s.TotalSize() != 10 {
		t.Errorf("TotalSize returned %d, want 10", s.TotalSize())
	}
}
