package fs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newLogBlobStore builds an FSStore with a LocalChunkIndex wired (the memory
// metadata store), so chunk persistence routes to the log-blob substrate.
// maxDisk is unbounded so the capacity gate never trips during the test.
func newLogBlobStore(t *testing.T) (*FSStore, context.Context) {
	t.Helper()
	dir := t.TempDir()
	idx := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc, err := NewWithOptions(dir, 0, idx, FSStoreOptions{})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	return bc, context.Background()
}

// TestReadChunk_TornLogBlobTail_TreatedAsMiss proves the read-through
// crash-consistency contract: when a log-blob tail is torn (the durable index
// entry survives a crash but the appended bytes never reached disk), ReadChunk
// must NOT hard-error. It must drop the dangling index entry and report the
// chunk as a clean miss (ErrChunkNotFound), so an upstream engine treats it
// exactly like an evicted chunk and refetches from the durable remote copy.
//
// A truncation of the blob file models the post-crash state: index committed,
// blob bytes lost. Before the fix, logBlob.ReadAt returned io.EOF and ReadChunk
// wrapped it into a hard error with no recovery route.
func TestReadChunk_TornLogBlobTail_TreatedAsMiss(t *testing.T) {
	bc, ctx := newLogBlobStore(t)

	data := []byte("read-through staged chunk that will be torn by a crash")
	h := blake3ContentHash(data)

	// Stage via Put (the read-through cache's write entry) — appends to the
	// log-blob and commits a durable index entry, WITHOUT fsyncing the blob.
	if err := bc.Put(ctx, h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Sanity: the chunk reads back cleanly before the tear.
	if got, err := bc.ReadChunk(ctx, h); err != nil || string(got) != string(data) {
		t.Fatalf("pre-tear ReadChunk = %q, err=%v; want %q", got, err, data)
	}
	if ok, err := bc.HasChunk(ctx, h); err != nil || !ok {
		t.Fatalf("pre-tear HasChunk = %v, err=%v; want true", ok, err)
	}

	// Simulate the torn tail: truncate the active blob so the recorded bytes
	// are no longer on disk (index entry is untouched — it's in the index
	// store, which "survived the crash").
	blobsDir := filepath.Join(bc.baseDir, "blobs")
	entries, err := os.ReadDir(blobsDir)
	if err != nil {
		t.Fatalf("read blobs dir: %v", err)
	}
	truncated := false
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".blob" {
			if err := os.Truncate(filepath.Join(blobsDir, e.Name()), 0); err != nil {
				t.Fatalf("truncate blob %s: %v", e.Name(), err)
			}
			truncated = true
		}
	}
	if !truncated {
		t.Fatal("no .blob file found to truncate")
	}

	// The torn read must surface as a clean miss, not a hard error.
	if _, err := bc.ReadChunk(ctx, h); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("torn ReadChunk err = %v; want ErrChunkNotFound (clean miss, no hard error)", err)
	}

	// The dangling index entry must be gone so HasChunk now reports absence —
	// this is what routes the engine's EnsureAvailableAndRead to refetch from
	// remote instead of assuming the chunk is all-local and returning zeros.
	if ok, err := bc.HasChunk(ctx, h); err != nil || ok {
		t.Fatalf("post-tear HasChunk = %v, err=%v; want false (torn entry dropped)", ok, err)
	}
}
