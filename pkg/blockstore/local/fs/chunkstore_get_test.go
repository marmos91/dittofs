package fs

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// TestFSStore_Get_DelegatesToReadChunk asserts that the new Get method
// exists on *FSStore with the LocalStore.Get signature, returns the
// stored bytes byte-identically, and reports blockstore.ErrChunkNotFound
// for missing hashes. This is the Phase 16 Plan 01 RED for the FSStore
// side of the new interface method.
func TestFSStore_Get_DelegatesToReadChunk(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: true})
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

	var missing blockstore.ContentHash
	if _, err := bc.Get(ctx, missing); !errors.Is(err, blockstore.ErrChunkNotFound) {
		t.Fatalf("Get(missing): want ErrChunkNotFound, got %v", err)
	}
}
