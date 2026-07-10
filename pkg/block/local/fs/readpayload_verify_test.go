package fs

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestReadPayloadAt_CorruptLocalChunk_DoesNotServeSilently is the warm-read
// data-integrity guard's fail-before/pass-after gate.
//
// It plants a CORRUPT local chunk (garbage bytes stored under a hash that is
// the blake3 of DIFFERENT, correct bytes) plus a FileChunk manifest row that
// references that hash, then reads through ReadPayloadAt — the local warm read
// path. Before the guard, fillFromCASManifest / fillFromCASManifestScan copy the
// unverified local bytes straight into dest and return (len, nil): the client
// receives silently-wrong data (the SMB warm-read bug). After the guard, the
// blake3 mismatch leaves the region uncovered so ReadPayloadAt returns a miss
// (block.ErrFileChunkNotFound), which the engine routes to the blake3-verified
// remote-fetch/heal path instead of serving corruption.
//
// Runs over memory (fillFromCASManifestScan) and badger (the indexed
// coveringChunkResolver fast path) so both local CAS read sites are gated.
func TestReadPayloadAt_CorruptLocalChunk_DoesNotServeSilently(t *testing.T) {
	t.Run("memory_scan", func(t *testing.T) {
		runCorruptLocalChunk(t, metadatamemory.NewMemoryMetadataStoreWithDefaults())
	})
	t.Run("badger_covering", func(t *testing.T) {
		ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), t.TempDir())
		if err != nil {
			t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
		}
		defer func() { _ = ms.Close() }()
		runCorruptLocalChunk(t, ms)
	})
}

func runCorruptLocalChunk(t *testing.T, ms metadata.Store) {
	t.Helper()
	ctx := context.Background()

	fcs, ok := ms.(block.EngineFileChunkStore)
	if !ok {
		t.Fatalf("metadata store %T does not implement block.EngineFileChunkStore", ms)
	}
	lci, ok := ms.(metadata.LocalChunkIndex)
	if !ok {
		t.Fatalf("metadata store %T does not implement metadata.LocalChunkIndex", ms)
	}

	store, err := NewWithOptions(t.TempDir(), 0, fcs, FSStoreOptions{LocalChunkIndex: lci})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The hash addresses the CORRECT bytes; the local store holds GARBAGE under
	// that hash (models on-disk / logblob corruption of a warm chunk).
	correct := bytes.Repeat([]byte("correct-payload-bytes!"), 64)
	h := block.ContentHash(blake3.Sum256(correct))
	garbage := bytes.Repeat([]byte{0xEE}, len(correct))
	if bytes.Equal(garbage, correct) {
		t.Fatal("test setup: garbage must differ from correct")
	}

	if err := store.StoreChunk(ctx, h, garbage); err != nil {
		t.Fatalf("StoreChunk (plant corrupt local bytes): %v", err)
	}
	// Manifest row for payload "p" at offset 0 referencing the (correct) hash.
	fb := &block.FileChunk{
		ID:       "p/0",
		Hash:     h,
		DataSize: uint32(len(correct)),
		State:    block.BlockStatePending,
	}
	if err := fcs.Put(ctx, fb); err != nil {
		t.Fatalf("FileChunk Put: %v", err)
	}

	dest := make([]byte, len(correct))
	n, err := store.ReadPayloadAt(ctx, "p", dest, 0)

	// After the guard: the blake3 mismatch is detected, the region is left
	// uncovered, and ReadPayloadAt surfaces a miss. The corrupt bytes must NOT
	// reach dest as a successful read.
	if !errors.Is(err, block.ErrFileChunkNotFound) {
		t.Fatalf("corrupt local chunk was served silently: ReadPayloadAt returned n=%d err=%v (dest==garbage: %v); want block.ErrFileChunkNotFound so the engine routes to the verified remote path",
			n, err, bytes.Equal(dest, garbage))
	}
}
