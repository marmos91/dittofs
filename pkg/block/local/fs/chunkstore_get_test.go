package fs

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// TestFSStore_Get_DelegatesToReadChunk asserts that the Get method
// exists on *FSStore with the LocalStore.Get signature, returns the
// stored bytes byte-identically, and reports block.ErrChunkNotFound
// for missing hashes.
func TestFSStore_Get_DelegatesToReadChunk(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	ctx := context.Background()
	h := hashFromHex(t, strings.Repeat("a1", 32))
	data := bytes.Repeat([]byte{0xA1}, 4096)

	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}

	got, err := bc.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("Get returned different bytes than StoreChunk wrote")
	}

	var missing block.ContentHash
	if _, err := bc.Get(ctx, missing); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("Get(missing): want ErrChunkNotFound, got %v", err)
	}
}
