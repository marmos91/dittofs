package fs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newLogBlobStore builds an FSStore with a LocalChunkIndex wired, so chunk
// persistence routes to the log-blob tier instead of per-chunk CAS files.
// maxDisk is unbounded so ensureSpace never evicts during the test.
func newLogBlobStore(t *testing.T) (*FSStore, *os.Root, context.Context) {
	t.Helper()
	dir := t.TempDir()
	idx := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc, err := NewWithOptions(dir, 0, nil, FSStoreOptions{
		LocalChunkIndex: idx,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	return bc, nil, context.Background()
}

// TestStoreChunk_RoutesToLogBlob proves that when a LocalChunkIndex is wired,
// StoreChunk writes the chunk bytes to the log-blob substrate and records the
// location in the index — it does NOT create a per-chunk cas/<hash> file.
func TestStoreChunk_RoutesToLogBlob(t *testing.T) {
	bc, _, ctx := newLogBlobStore(t)

	data := []byte("hello log-blob substrate")
	h := blake3ContentHash(data)

	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}

	// No legacy CAS file must be created for a new logblob-backed write.
	if _, err := os.Stat(bc.chunkPath(h)); !os.IsNotExist(err) {
		t.Fatalf("expected no CAS file at %s, stat err=%v", bc.chunkPath(h), err)
	}

	// The local chunk index must record the location.
	loc, ok, err := bc.localChunkIndex.GetLocalLocation(ctx, h)
	if err != nil {
		t.Fatalf("GetLocalLocation: %v", err)
	}
	if !ok {
		t.Fatalf("expected index hit for stored chunk, got miss")
	}
	if loc.RawLength != int64(len(data)) {
		t.Fatalf("index RawLength = %d, want %d", loc.RawLength, len(data))
	}

	// A .blob file must exist under <baseDir>/blobs/.
	entries, err := os.ReadDir(filepath.Join(bc.baseDir, "blobs"))
	if err != nil {
		t.Fatalf("read blobs dir: %v", err)
	}
	var sawBlob bool
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".blob" {
			sawBlob = true
		}
	}
	if !sawBlob {
		t.Fatalf("expected a .blob file under blobs/, found %v", entries)
	}

	// Round-trips byte-identical via ReadChunk (index hit -> logblob.ReadAt).
	got, err := bc.ReadChunk(ctx, h)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("ReadChunk = %q, want %q", got, data)
	}
}

// TestReadChunk_LegacyCASBackCompat proves that a pre-existing cas/<hash> file
// (no index entry) stays readable on a logblob-backed store — the retained
// legacy CAS read path. Removed only in PR4.
func TestReadChunk_LegacyCASBackCompat(t *testing.T) {
	bc, _, ctx := newLogBlobStore(t)

	legacy := []byte("legacy cas chunk bytes")
	h := blake3ContentHash(legacy)

	// Write a CAS file directly, bypassing StoreChunk, with NO index entry.
	path := bc.chunkPath(h)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, legacy, 0o644); err != nil {
		t.Fatalf("write legacy cas file: %v", err)
	}

	// Index miss -> CAS fallback reports existence.
	ok, err := bc.HasChunk(ctx, h)
	if err != nil {
		t.Fatalf("HasChunk: %v", err)
	}
	if !ok {
		t.Fatalf("HasChunk = false for pre-existing CAS file, want true")
	}

	// Index miss -> CAS fallback returns the bytes.
	got, err := bc.ReadChunk(ctx, h)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if string(got) != string(legacy) {
		t.Fatalf("ReadChunk = %q, want %q", got, legacy)
	}
}
