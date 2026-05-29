//go:build integration

package postgres_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// hashOfSeed returns a deterministic ContentHash for the given seed string.
// (Mirrors the storetest helper of the same name; duplicated here because
// integration tests live in package postgres_test, not storetest.)
func hashOfSeed(seed string) blockstore.ContentHash {
	sum := sha256.Sum256([]byte(seed))
	var h blockstore.ContentHash
	copy(h[:], sum[:])
	return h
}

// newTestStore opens a fresh PostgresMetadataStore for the test. The
// underlying database is shared across tests; each test should use unique
// share names to avoid interference.
func newTestStore(t *testing.T) metadata.MetadataStore {
	t.Helper()

	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL test")
	}

	cfg := &postgres.PostgresMetadataStoreConfig{
		Host:        "localhost",
		Port:        5432,
		Database:    "dittofs_test",
		User:        "postgres",
		Password:    "postgres",
		SSLMode:     "disable",
		AutoMigrate: true,
	}
	caps := metadata.FilesystemCapabilities{
		MaxReadSize:         1048576,
		PreferredReadSize:   1048576,
		MaxWriteSize:        1048576,
		PreferredWriteSize:  1048576,
		MaxFileSize:         9223372036854775807,
		MaxFilenameLen:      255,
		MaxPathLen:          4096,
		MaxHardLinkCount:    32767,
		SupportsHardLinks:   true,
		SupportsSymlinks:    true,
		CaseSensitive:       true,
		CasePreserving:      true,
		TimestampResolution: 1,
	}
	store, err := postgres.NewPostgresMetadataStore(context.Background(), cfg, caps)
	if err != nil {
		t.Fatalf("NewPostgresMetadataStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// createShareAndFile is a local helper mirroring storetest.createTestShare/createTestFile
// (those helpers are unexported in the storetest package).
func createShareAndFile(t *testing.T, store metadata.MetadataStore, shareName, fileName string) metadata.FileHandle {
	t.Helper()
	ctx := t.Context()

	// CreateRootDirectory creates both the files row and the shares row
	// (via ON CONFLICT in transaction.go's CreateRootDirectory). We skip
	// the standalone CreateShare call because the postgres backend's
	// CreateShare INSERT does not include root_file_id (pre-existing
	// scope-boundary issue, not introduced by).
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory(%q): %v", shareName, err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle(root): %v", err)
	}

	handle, err := store.GenerateHandle(ctx, shareName, "/"+fileName)
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	file := &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      "/" + fileName,
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
			UID:  1000,
			GID:  1000,
		},
	}
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if err := store.SetParent(ctx, handle, rootHandle); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if err := store.SetChild(ctx, rootHandle, fileName, handle); err != nil {
		t.Fatalf("SetChild: %v", err)
	}
	if err := store.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount: %v", err)
	}
	return handle
}

// TestPostgres_FileBlockRefs_BlocksRoundTrip asserts that PutFile with a
// non-empty Blocks list, followed by GetFile, returns identical Blocks
// (sorted by Offset, byte-equal Hash, equal Offset, equal Size).
func TestPostgres_FileBlockRefs_BlocksRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()

	handle := createShareAndFile(t, store, "/blockref-roundtrip", "vm.img")

	want := []blockstore.BlockRef{
		{Hash: hashOfSeed("ref-0"), Offset: 0, Size: 4 << 20},
		{Hash: hashOfSeed("ref-1"), Offset: 4 << 20, Size: 4 << 20},
		{Hash: hashOfSeed("ref-2"), Offset: 8 << 20, Size: 1 << 20},
	}

	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile (initial): %v", err)
	}
	file.FileAttr.Blocks = want
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile with Blocks: %v", err)
	}

	got, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile (round-trip): %v", err)
	}
	if len(got.FileAttr.Blocks) != len(want) {
		t.Fatalf("Blocks: got %d entries, want %d", len(got.FileAttr.Blocks), len(want))
	}
	for i, w := range want {
		g := got.FileAttr.Blocks[i]
		if !bytes.Equal(g.Hash[:], w.Hash[:]) {
			t.Errorf("Blocks[%d].Hash mismatch:\n got  %x\n want %x", i, g.Hash[:], w.Hash[:])
		}
		if g.Offset != w.Offset {
			t.Errorf("Blocks[%d].Offset = %d, want %d", i, g.Offset, w.Offset)
		}
		if g.Size != w.Size {
			t.Errorf("Blocks[%d].Size = %d, want %d", i, g.Size, w.Size)
		}
	}
}

// TestPostgres_FileBlockRefs_ReplaceFully asserts that a second PutFile
// with a different Blocks list fully replaces the first (no leftover rows).
func TestPostgres_FileBlockRefs_ReplaceFully(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()

	handle := createShareAndFile(t, store, "/blockref-replace", "vm.img")

	first := []blockstore.BlockRef{
		{Hash: hashOfSeed("first-0"), Offset: 0, Size: 4 << 20},
		{Hash: hashOfSeed("first-1"), Offset: 4 << 20, Size: 4 << 20},
		{Hash: hashOfSeed("first-2"), Offset: 8 << 20, Size: 4 << 20},
	}
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile (1): %v", err)
	}
	file.FileAttr.Blocks = first
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile (first): %v", err)
	}

	// Second write with a different (and shorter) list.
	second := []blockstore.BlockRef{
		{Hash: hashOfSeed("second-0"), Offset: 0, Size: 2 << 20},
		{Hash: hashOfSeed("second-1"), Offset: 2 << 20, Size: 2 << 20},
	}
	file, err = store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile (2): %v", err)
	}
	file.FileAttr.Blocks = second
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile (second): %v", err)
	}

	got, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile (round-trip): %v", err)
	}
	if len(got.FileAttr.Blocks) != len(second) {
		t.Fatalf("Blocks: got %d, want %d (no leftover rows from first list)",
			len(got.FileAttr.Blocks), len(second))
	}
	for i, w := range second {
		g := got.FileAttr.Blocks[i]
		if !bytes.Equal(g.Hash[:], w.Hash[:]) || g.Offset != w.Offset || g.Size != w.Size {
			t.Errorf("Blocks[%d] = %+v, want %+v", i, g, w)
		}
	}
}

// TestPostgres_FileBlockRefs_CascadeDelete asserts that deleting a file row
// cascades to file_block_refs (FK ON DELETE CASCADE).
func TestPostgres_FileBlockRefs_CascadeDelete(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()

	handle := createShareAndFile(t, store, "/blockref-cascade", "vm.img")

	// Persist Blocks.
	blocks := []blockstore.BlockRef{
		{Hash: hashOfSeed("cas-0"), Offset: 0, Size: 4 << 20},
		{Hash: hashOfSeed("cas-1"), Offset: 4 << 20, Size: 4 << 20},
	}
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	file.FileAttr.Blocks = blocks
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	// Capture file ID for direct SQL count.
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}

	// Pre-delete: 2 rows expected.
	rawSQL, ok := store.(postgres.RawSQLAccessor)
	if !ok {
		t.Fatalf("store does not implement RawSQLAccessor — cannot count file_block_refs rows")
	}
	pre, err := rawSQL.CountFileBlockRefs(ctx, fileID)
	if err != nil {
		t.Fatalf("CountFileBlockRefs (pre): %v", err)
	}
	if pre != 2 {
		t.Fatalf("pre-delete row count = %d, want 2", pre)
	}

	// Delete child mapping then file (matches storetest pattern).
	if err := store.DeleteChild(ctx, mustParentHandle(t, store, handle), "vm.img"); err != nil {
		t.Fatalf("DeleteChild: %v", err)
	}
	if err := store.DeleteFile(ctx, handle); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	// Post-delete: 0 rows expected (FK cascade).
	post, err := rawSQL.CountFileBlockRefs(ctx, fileID)
	if err != nil {
		t.Fatalf("CountFileBlockRefs (post): %v", err)
	}
	if post != 0 {
		t.Fatalf("post-delete row count = %d, want 0 (cascade should have cleaned up)", post)
	}
}

func mustParentHandle(t *testing.T, store metadata.MetadataStore, handle metadata.FileHandle) metadata.FileHandle {
	t.Helper()
	parent, err := store.GetParent(t.Context(), handle)
	if err != nil {
		t.Fatalf("GetParent: %v", err)
	}
	return parent
}

// TestPostgres_FileBlockRefs_ConcurrentPutFile asserts that two concurrent
// PutFile calls on the same file_id do not produce duplicate or interleaved
// rows. The PK (file_id, offset) means duplicates would error; the test
// asserts no error AND a final state matching one of the two writers.
func TestPostgres_FileBlockRefs_ConcurrentPutFile(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()

	handle := createShareAndFile(t, store, "/blockref-concurrent", "vm.img")

	a := []blockstore.BlockRef{
		{Hash: hashOfSeed("a-0"), Offset: 0, Size: 4 << 20},
		{Hash: hashOfSeed("a-1"), Offset: 4 << 20, Size: 4 << 20},
	}
	b := []blockstore.BlockRef{
		{Hash: hashOfSeed("b-0"), Offset: 0, Size: 2 << 20},
		{Hash: hashOfSeed("b-1"), Offset: 2 << 20, Size: 2 << 20},
		{Hash: hashOfSeed("b-2"), Offset: 4 << 20, Size: 2 << 20},
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, blocks := range [][]blockstore.BlockRef{a, b} {
		wg.Add(1)
		go func(idx int, blocks []blockstore.BlockRef) {
			defer wg.Done()
			file, err := store.GetFile(ctx, handle)
			if err != nil {
				errs[idx] = err
				return
			}
			file.FileAttr.Blocks = blocks
			errs[idx] = store.PutFile(ctx, file)
		}(i, blocks)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("PutFile goroutine %d: %v", i, err)
		}
	}

	// Final state must match exactly one of {a, b} — no interleaving.
	got, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile (final): %v", err)
	}
	final := got.FileAttr.Blocks
	matchA := blockRefsEqual(final, a)
	matchB := blockRefsEqual(final, b)
	if !matchA && !matchB {
		t.Fatalf("final Blocks neither a nor b: got %+v\n  want a=%+v\n  or   b=%+v", final, a, b)
	}
}

func blockRefsEqual(x, y []blockstore.BlockRef) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if !bytes.Equal(x[i].Hash[:], y[i].Hash[:]) {
			return false
		}
		if x[i].Offset != y[i].Offset || x[i].Size != y[i].Size {
			return false
		}
	}
	return true
}

// TestPostgres_Restore_ReconcilesNullHashFileBlocks pins the data-loss fix:
// a backup that carries NULL-hash file_blocks rows (the shape every backup
// had before the Put hash-gate fix) must, after restore, have those hashes
// backfilled from file_block_refs. Otherwise the engine's CAS read path
// resolves the per-file read index to a NULL hash and the restored file
// reads as zeros once local cache state is cold.
func TestPostgres_Restore_ReconcilesNullHashFileBlocks(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()

	handle := createShareAndFile(t, store, "/restore-reconcile", "vm.img")

	want := hashOfSeed("reconcile-block-0")
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	// content_id (PayloadID) is the prefix of the file_blocks row ID. The
	// engine derives it from share + path; set it explicitly here so the
	// reconcile join (file_blocks.id = content_id || '/' || offset) lands.
	payloadID := "restore-reconcile/vm.img"
	file.PayloadID = metadata.PayloadID(payloadID)
	file.Blocks = []blockstore.BlockRef{
		{Hash: want, Offset: 0, Size: 4 << 20},
	}
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile with Blocks: %v", err)
	}

	// The file_blocks read-index row ID is "{content_id}/{offset}".
	blockID := payloadID + "/0"

	rawSQL, ok := store.(postgres.RawSQLAccessor)
	if !ok {
		t.Fatalf("store does not implement RawSQLAccessor")
	}

	// Simulate a legacy backup: a NULL-hash file_blocks row.
	if err := rawSQL.InsertNullHashFileBlock(ctx, blockID, 4<<20); err != nil {
		t.Fatalf("InsertNullHashFileBlock: %v", err)
	}
	if got, herr := rawSQL.FileBlockHashHex(ctx, blockID); herr != nil {
		t.Fatalf("FileBlockHashHex (pre-backup): %v", herr)
	} else if got != "" {
		t.Fatalf("pre-backup file_blocks.hash = %q, want NULL", got)
	}

	backupable, ok := store.(metadata.Backupable)
	if !ok {
		t.Fatalf("store does not implement Backupable")
	}

	var buf bytes.Buffer
	if _, err := backupable.Backup(ctx, &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	resetable, ok := store.(interface {
		Reset(ctx context.Context) error
	})
	if !ok {
		t.Fatalf("store does not implement Reset")
	}
	if err := resetable.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if err := backupable.Restore(ctx, &buf); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// After restore, the NULL hash must be reconciled from file_block_refs.
	got, err := rawSQL.FileBlockHashHex(ctx, blockID)
	if err != nil {
		t.Fatalf("FileBlockHashHex (post-restore): %v", err)
	}
	if got != want.String() {
		t.Errorf("post-restore file_blocks.hash = %q, want %q "+
			"(restore must backfill NULL hashes from file_block_refs so the "+
			"CAS read path can resolve restored chunks)", got, want.String())
	}
}
