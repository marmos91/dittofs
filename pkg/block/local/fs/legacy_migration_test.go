package fs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// writeLegacyChunkFile plants a pre-flip per-chunk file at the legacy
// content-addressed path <baseDir>/blocks/<hh>/<hh>/<hex> without going
// through the store (the legacy writer is gone; migration is the only
// consumer of this layout).
func writeLegacyChunkFile(t *testing.T, baseDir string, data []byte) string {
	t.Helper()
	h := blake3ContentHash(data)
	hex := h.String()
	dir := filepath.Join(baseDir, "blocks", hex[0:2], hex[2:4])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir legacy shard: %v", err)
	}
	path := filepath.Join(dir, hex)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write legacy chunk: %v", err)
	}
	return path
}

// TestMigrateLegacyChunkFiles_ImportsAndRemoves proves the local migration
// phase: every legacy per-chunk file is imported into the log-blob substrate
// (readable by hash afterwards), the file is removed, and the shard dirs are
// pruned.
func TestMigrateLegacyChunkFiles_ImportsAndRemoves(t *testing.T) {
	dir := t.TempDir()
	// Plant the legacy files BEFORE opening the store so this models a real
	// pre-flip dataset (seedLRUFromDisk sees them, like a production restart).
	payloads := [][]byte{
		[]byte("legacy chunk one"),
		[]byte("legacy chunk two — a little longer to vary sizes"),
		bytes.Repeat([]byte{0xAB}, 8192),
	}
	var paths []string
	for _, p := range payloads {
		paths = append(paths, writeLegacyChunkFile(t, dir, p))
	}

	bc, ctx := newLogBlobStoreAt(t, dir)

	n, err := bc.MigrateLegacyChunkFiles(ctx)
	if err != nil {
		t.Fatalf("MigrateLegacyChunkFiles: %v", err)
	}
	if n != len(payloads) {
		t.Fatalf("migrated = %d, want %d", n, len(payloads))
	}

	for i, p := range payloads {
		h := blake3ContentHash(p)
		got, err := bc.ReadChunk(ctx, h)
		if err != nil {
			t.Fatalf("ReadChunk(%d) after migration: %v", i, err)
		}
		if !bytes.Equal(got, p) {
			t.Fatalf("chunk %d not byte-identical after migration", i)
		}
		if _, ok, _ := bc.localChunkIndex.GetLocalLocation(ctx, h); !ok {
			t.Fatalf("chunk %d missing from local chunk index", i)
		}
		if _, err := os.Stat(paths[i]); !os.IsNotExist(err) {
			t.Fatalf("legacy file %d still present: %v", i, err)
		}
	}

	// Shard dirs pruned (blocks/ may be fully removed).
	if entries, err := os.ReadDir(filepath.Join(dir, "blocks")); err == nil && len(entries) != 0 {
		t.Fatalf("legacy shard dirs not pruned: %d entries left", len(entries))
	}

	// Idempotent: a second run finds nothing.
	n, err = bc.MigrateLegacyChunkFiles(ctx)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if n != 0 {
		t.Fatalf("re-run migrated %d chunks, want 0", n)
	}
}

// TestMigrateLegacyChunkFiles_DedupsAgainstIndex proves resumability: a chunk
// already imported (index hit) has its file consumed without a duplicate
// append.
func TestMigrateLegacyChunkFiles_DedupsAgainstIndex(t *testing.T) {
	dir := t.TempDir()
	data := []byte("already imported chunk")
	path := writeLegacyChunkFile(t, dir, data)

	bc, ctx := newLogBlobStoreAt(t, dir)
	h := blake3ContentHash(data)

	// Simulate the crash-after-import-before-remove window: chunk is in the
	// substrate, file still on disk.
	if err := bc.storeChunkLogBlob(ctx, h, data); err != nil {
		t.Fatalf("storeChunkLogBlob: %v", err)
	}
	locBefore, ok, _ := bc.localChunkIndex.GetLocalLocation(ctx, h)
	if !ok {
		t.Fatal("precondition: chunk should be indexed")
	}

	if _, err := bc.MigrateLegacyChunkFiles(ctx); err != nil {
		t.Fatalf("MigrateLegacyChunkFiles: %v", err)
	}

	locAfter, ok, _ := bc.localChunkIndex.GetLocalLocation(ctx, h)
	if !ok {
		t.Fatal("chunk lost from index")
	}
	if locBefore != locAfter {
		t.Fatalf("location changed on dedup import: %+v -> %+v", locBefore, locAfter)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("legacy file not consumed on dedup")
	}
	got, err := bc.ReadChunk(ctx, h)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("chunk unreadable after dedup import: %v", err)
	}
}

// TestMigrateLegacyChunkFiles_CorruptFileLeftInPlace proves fail-safe
// handling: a legacy file whose bytes do not hash to its name is left on disk
// for inspection, is not imported, and does not abort the migration.
func TestMigrateLegacyChunkFiles_CorruptFileLeftInPlace(t *testing.T) {
	dir := t.TempDir()
	good := []byte("good chunk")
	goodPath := writeLegacyChunkFile(t, dir, good)
	corruptPath := writeLegacyChunkFile(t, dir, []byte("original bytes"))
	if err := os.WriteFile(corruptPath, []byte("tampered bytes!"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	bc, ctx := newLogBlobStoreAt(t, dir)
	n, err := bc.MigrateLegacyChunkFiles(ctx)
	if err != nil {
		t.Fatalf("MigrateLegacyChunkFiles: %v", err)
	}
	if n != 1 {
		t.Fatalf("migrated = %d, want 1 (corrupt file skipped)", n)
	}
	if _, err := os.Stat(corruptPath); err != nil {
		t.Fatalf("corrupt file should be left in place: %v", err)
	}
	if _, err := os.Stat(goodPath); !os.IsNotExist(err) {
		t.Fatal("good file should be consumed")
	}
}

// TestMigrateLegacyChunkFiles_IgnoresForeignFiles proves the walk only
// consumes hash-named files: tmp leftovers and other artifacts are untouched.
func TestMigrateLegacyChunkFiles_IgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "blocks", "ab", "cd")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	tmpPath := filepath.Join(foreign, "deadbeef.tmp-1234")
	if err := os.WriteFile(tmpPath, []byte("tmp leftovers"), 0o644); err != nil {
		t.Fatal(err)
	}

	bc, ctx := newLogBlobStoreAt(t, dir)
	n, err := bc.MigrateLegacyChunkFiles(ctx)
	if err != nil {
		t.Fatalf("MigrateLegacyChunkFiles: %v", err)
	}
	if n != 0 {
		t.Fatalf("migrated = %d, want 0", n)
	}
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("foreign file should be untouched: %v", err)
	}
}

// newLogBlobStoreAt mirrors newLogBlobStore but over a caller-owned dir so
// legacy files can be planted before the store opens.
func newLogBlobStoreAt(t *testing.T, dir string) (*FSStore, context.Context) {
	t.Helper()
	bc, err := NewWithOptions(dir, 0, memmeta.NewMemoryMetadataStoreWithDefaults(), FSStoreOptions{})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	return bc, context.Background()
}
