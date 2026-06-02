package trash

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Test harness
// ============================================================================

// stubTrashPolicy enables trash for every share so MetadataService recycles on
// RemoveFile/RemoveDirectory instead of destroying content.
type stubTrashPolicy struct {
	cfg metadata.TrashConfig
}

func (p stubTrashPolicy) TrashConfigForShare(string) (metadata.TrashConfig, bool) {
	return p.cfg, true
}

// testDeps is an in-test implementation of Deps backed by a single real
// in-memory MetadataService. FreeBlocks records the payloadIDs it is asked to
// free so tests can assert block reclamation happens for permanent deletes.
type testDeps struct {
	shareName  string
	svc        *metadata.Service
	rootHandle metadata.FileHandle
	cfg        Config
	freed      []string
	// freedBlocks records the BlockRef count threaded into each FreeBlocks call,
	// keyed by payloadID, so tests can assert the purge path passes the file's
	// blocks (not nil) — the CAS RefCounts are only decremented when blocks flow
	// through, so a regression to nil would leak them (#832).
	freedBlocks map[string]int
}

func (d *testDeps) MetadataServiceForShare(shareName string) (*metadata.Service, metadata.FileHandle, bool) {
	if shareName != d.shareName {
		return nil, nil, false
	}
	return d.svc, d.rootHandle, true
}

func (d *testDeps) TrashConfigForShare(shareName string) (Config, bool) {
	if shareName != d.shareName {
		return Config{}, false
	}
	return d.cfg, true
}

func (d *testDeps) EnabledTrashShares() []string {
	return []string{d.shareName}
}

func (d *testDeps) FreeBlocks(_ context.Context, _ string, _ metadata.FileHandle, payloadID string, blocks []block.BlockRef) error {
	if payloadID == "" {
		return nil
	}
	d.freed = append(d.freed, payloadID)
	if d.freedBlocks == nil {
		d.freedBlocks = make(map[string]int)
	}
	d.freedBlocks[payloadID] = len(blocks)
	return nil
}

// trashTest bundles the service under test, its deps, and the auth context the
// subtests act as.
type trashTest struct {
	t    *testing.T
	svc  *Service
	deps *testDeps
	ctx  *metadata.AuthContext
}

// newTestTrash builds a real in-memory MetadataService with trash enabled
// (mirroring pkg/metadata/file_recycle_test.go's newRecycleFixture ordering:
// CreateShare BEFORE CreateRootDirectory + SetTrashPolicy), wraps it in a Deps
// impl, and constructs the trash.Service over it.
func newTestTrash(t *testing.T) *trashTest {
	t.Helper()

	store := memory.NewMemoryMetadataStoreWithDefaults()
	bg := context.Background()
	shareName := "/test"

	require.NoError(t, store.CreateShare(bg, &metadata.Share{Name: shareName}))
	rootFile, err := store.CreateRootDirectory(bg, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o777,
	})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	require.NoError(t, err)

	msvc := metadata.New()
	require.NoError(t, msvc.RegisterStoreForShare(shareName, store))
	msvc.SetTrashPolicy(stubTrashPolicy{cfg: metadata.TrashConfig{Enabled: true}})

	deps := &testDeps{
		shareName:  shareName,
		svc:        msvc,
		rootHandle: rootHandle,
		cfg:        Config{Enabled: true},
	}

	return &trashTest{
		t:    t,
		svc:  New(deps, 0),
		deps: deps,
		ctx:  rootAuthContext(),
	}
}

// rootAuthContext returns a root (uid/gid 0) AuthContext.
func rootAuthContext() *metadata.AuthContext {
	return &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(0),
			GID:  metadata.Uint32Ptr(0),
			GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}
}

// createFile creates a regular file under parent.
func (tt *trashTest) createFile(parent metadata.FileHandle, name string) metadata.FileHandle {
	tt.t.Helper()
	_, _, err := tt.deps.svc.CreateFile(tt.ctx, parent, name, &metadata.FileAttr{Mode: 0o644})
	require.NoError(tt.t, err)
	h, err := tt.deps.svc.GetChild(tt.ctx.Context, parent, name)
	require.NoError(tt.t, err)
	return h
}

// mkdir creates a directory under parent and returns its handle.
func (tt *trashTest) mkdir(parent metadata.FileHandle, name string) metadata.FileHandle {
	tt.t.Helper()
	dir, _, err := tt.deps.svc.CreateDirectory(tt.ctx, parent, name, &metadata.FileAttr{Mode: 0o755})
	require.NoError(tt.t, err)
	h, err := metadata.EncodeFileHandle(dir)
	require.NoError(tt.t, err)
	return h
}

// recycle creates a file at the share root and RemoveFile's it so it lands in
// the bin, then returns the recycled entry's BinPath (its name under #recycle).
func (tt *trashTest) recycle(name string) {
	tt.t.Helper()
	tt.createFile(tt.deps.rootHandle, name)
	_, _, err := tt.deps.svc.RemoveFile(tt.ctx, tt.deps.rootHandle, name)
	require.NoError(tt.t, err)
}

// recycleAt creates a file at share-relative dir/name (creating dir if needed)
// and RemoveFile's it so it lands in the bin under #recycle/dir/name, exercising
// the recreated intermediary-parent chain.
func (tt *trashTest) recycleAt(dir, name string) {
	tt.t.Helper()
	parent := tt.deps.rootHandle
	if dir != "" {
		if h, err := tt.deps.svc.GetChild(tt.ctx.Context, tt.deps.rootHandle, dir); err == nil {
			parent = h
		} else {
			parent = tt.mkdir(tt.deps.rootHandle, dir)
		}
	}
	tt.createFile(parent, name)
	_, _, err := tt.deps.svc.RemoveFile(tt.ctx, parent, name)
	require.NoError(tt.t, err)
}

// binHandle resolves the share's #recycle directory handle.
func (tt *trashTest) binHandle() metadata.FileHandle {
	tt.t.Helper()
	h, err := tt.deps.svc.GetChild(tt.ctx.Context, tt.deps.rootHandle, metadata.RecycleDirName)
	require.NoError(tt.t, err)
	return h
}

// isAlreadyExists reports whether err is a StoreError with the AlreadyExists
// code. The metadata package surfaces "destination exists" as a coded
// StoreError rather than a sentinel error value.
func isAlreadyExists(err error) bool {
	var se *metadata.StoreError
	return stderrors.As(err, &se) && se.Code == metadata.ErrAlreadyExists
}

// ============================================================================
// Tests
// ============================================================================

func TestRestoreClearsDeletionMetadata(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycle("doc.txt")

	require.NoError(t, tt.svc.Restore(tt.ctx, tt.deps.shareName, "doc.txt", ""))

	// Restored file is live at its original path with deletion metadata cleared.
	restored, err := tt.deps.svc.Lookup(tt.ctx, tt.deps.rootHandle, "doc.txt")
	require.NoError(t, err)
	assert.Nil(t, restored.DeletedAt, "restored file must not be marked deleted")
	assert.Empty(t, restored.OriginalPath)
	assert.Empty(t, restored.DeletedBy)

	// The bin no longer holds it.
	_, err = tt.deps.svc.Lookup(tt.ctx, tt.binHandle(), "doc.txt")
	assert.True(t, metadata.IsNotFoundError(err))
}

func TestRestoreConflictReturnsErrExist(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycle("doc.txt")
	// Recreate a live "doc.txt" at the root: restore must not clobber it.
	tt.createFile(tt.deps.rootHandle, "doc.txt")

	err := tt.svc.Restore(tt.ctx, tt.deps.shareName, "doc.txt", "")
	require.Error(t, err)
	assert.True(t, isAlreadyExists(err), "expected AlreadyExists, got %v", err)

	// The recycled copy is left untouched in the bin.
	binned, err := tt.deps.svc.Lookup(tt.ctx, tt.binHandle(), "doc.txt")
	require.NoError(t, err)
	require.NotNil(t, binned.DeletedAt)
}

func TestListReturnsRecycledRoots(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycle("a.txt")
	tt.recycle("b.txt")

	// A non-empty directory subtree recycles as ONE entry.
	dir := tt.mkdir(tt.deps.rootHandle, "docs")
	tt.createFile(dir, "inner.txt")
	_, rmErr := tt.deps.svc.RemoveDirectory(tt.ctx, tt.deps.rootHandle, "docs")
	require.NoError(t, rmErr)

	entries, err := tt.svc.List(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	require.Len(t, entries, 3, "expected 2 files + 1 subtree root, got %+v", entries)

	byOrig := make(map[string]Entry, len(entries))
	for _, e := range entries {
		assert.False(t, e.DeletedAt.IsZero(), "entry %q missing DeletedAt", e.BinPath)
		byOrig[e.OriginalPath] = e
	}
	require.Contains(t, byOrig, "a.txt")
	require.Contains(t, byOrig, "b.txt")
	require.Contains(t, byOrig, "docs")
	assert.True(t, byOrig["docs"].IsDir, "docs should be reported as a directory")
	assert.False(t, byOrig["a.txt"].IsDir)

	// The subtree child must NOT appear as its own top-level entry.
	for _, e := range entries {
		assert.NotEqual(t, "docs/inner.txt", e.OriginalPath)
	}
}

func TestEmptyRemovesAllBinEntries(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycle("a.txt")
	tt.recycle("b.txt")

	n, err := tt.svc.Empty(tt.ctx, tt.deps.shareName, false)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	// The bin is empty afterwards.
	entries, err := tt.svc.List(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	assert.Empty(t, entries)

	// FreeBlocks was invoked once per permanently-removed file.
	assert.Len(t, tt.deps.freed, 2, "each emptied file should free its blocks")
}

// TestEmptyThreadsBlocksToFreeBlocks guards the purge path against regressing to
// a nil BlockRef list: blockStore.Delete only decrements per-block CAS RefCounts
// (so GC can reclaim now-unreferenced chunks) when handed the file's blocks.
// Passing nil would leak the refcounts (#832). The test recycles a file carrying
// blocks and asserts the count threaded into FreeBlocks matches.
func TestEmptyThreadsBlocksToFreeBlocks(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	// Create a file and stamp two blocks + a payloadID directly through the
	// store (the in-memory service has no write path that synthesizes blocks).
	tt.createFile(tt.deps.rootHandle, "withblocks.txt")
	store, err := tt.deps.svc.GetStoreForShare(tt.deps.shareName)
	require.NoError(t, err)
	handle, err := tt.deps.svc.GetChild(tt.ctx.Context, tt.deps.rootHandle, "withblocks.txt")
	require.NoError(t, err)
	file, err := store.GetFile(tt.ctx.Context, handle)
	require.NoError(t, err)
	file.PayloadID = "payload-withblocks"
	file.Blocks = []block.BlockRef{
		{Offset: 0, Size: 4096},
		{Offset: 4096, Size: 4096},
	}
	require.NoError(t, store.PutFile(tt.ctx.Context, file))

	// Recycle it, then empty the bin.
	_, _, err = tt.deps.svc.RemoveFile(tt.ctx, tt.deps.rootHandle, "withblocks.txt")
	require.NoError(t, err)
	n, err := tt.svc.Empty(tt.ctx, tt.deps.shareName, false)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// The purge must thread the file's two blocks into FreeBlocks, not nil.
	assert.Equal(t, 2, tt.deps.freedBlocks["payload-withblocks"],
		"purge must pass the removed file's BlockRefs so CAS refcounts are decremented")
}

func TestListUnknownShareReturnsNotFound(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	_, err := tt.svc.List(tt.ctx, "/nope")
	assert.True(t, metadata.IsNotFoundError(err))
}

// TestListReturnsNestedEntries covers two files recycled from a shared original
// subdir: they land under #recycle/a/file1.txt and #recycle/a/file2.txt. List
// must return BOTH as entries (with the nested BinPath) and must NOT report the
// recreated intermediary "a" dir as its own entry.
func TestListReturnsNestedEntries(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycleAt("a", "file1.txt")
	tt.recycleAt("a", "file2.txt")

	entries, err := tt.svc.List(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	require.Len(t, entries, 2, "expected both nested files, got %+v", entries)

	byBin := make(map[string]Entry, len(entries))
	for _, e := range entries {
		byBin[e.BinPath] = e
	}
	require.Contains(t, byBin, "a/file1.txt")
	require.Contains(t, byBin, "a/file2.txt")
	assert.Equal(t, "a/file1.txt", byBin["a/file1.txt"].OriginalPath)
	assert.Equal(t, "a/file2.txt", byBin["a/file2.txt"].OriginalPath)

	// The intermediary "a" directory must never be listed as an entry.
	for _, e := range entries {
		assert.NotEqual(t, "a", e.BinPath, "intermediary dir must not be an entry")
		assert.False(t, e.IsDir, "nested entries are files, not the parent dir")
	}
}

// TestEmptyPrunesOrphanIntermediaryDirs verifies that after recycling a file
// from a deep original path and then Empty, the recreated intermediary "a"
// directory is swept out of #recycle (no orphan left behind), while #recycle
// itself remains.
func TestEmptyPrunesOrphanIntermediaryDirs(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycleAt("a", "b.txt")

	// Sanity: the intermediary "a" dir exists under #recycle before Empty.
	_, err := tt.deps.svc.GetChild(tt.ctx.Context, tt.binHandle(), "a")
	require.NoError(t, err, "intermediary dir should exist before Empty")

	n, err := tt.svc.Empty(tt.ctx, tt.deps.shareName, false)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// No orphan "a" dir remains under #recycle.
	_, err = tt.deps.svc.GetChild(tt.ctx.Context, tt.binHandle(), "a")
	assert.True(t, metadata.IsNotFoundError(err), "orphan intermediary dir must be pruned")

	// The bin is logically empty.
	entries, err := tt.svc.List(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	assert.Empty(t, entries)

	// #recycle root itself survives (only OnDisable removes it).
	_, err = tt.deps.svc.GetChild(tt.ctx.Context, tt.deps.rootHandle, metadata.RecycleDirName)
	assert.NoError(t, err, "#recycle root must survive Empty")
}

func TestStatusReportsCountsSizesAndOldest(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	// Empty bin: enabled, zero counts, no oldest.
	st, err := tt.svc.Status(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	assert.True(t, st.Enabled)
	assert.Equal(t, 0, st.ItemCount)
	assert.Equal(t, uint64(0), st.TotalBytes)
	assert.Nil(t, st.Oldest)

	// Recycle two sized files; the first recycled is the oldest.
	tt.recycleSized("first.txt", 100)
	tt.recycleSized("second.txt", 250)

	st, err = tt.svc.Status(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	assert.True(t, st.Enabled)
	assert.Equal(t, 2, st.ItemCount)
	assert.Equal(t, uint64(350), st.TotalBytes)
	require.NotNil(t, st.Oldest)

	// Oldest must equal the minimum DeletedAt across the listed entries.
	entries, err := tt.svc.List(tt.ctx, tt.deps.shareName)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	min := entries[0].DeletedAt
	for i := range entries {
		if entries[i].DeletedAt.Before(min) {
			min = entries[i].DeletedAt
		}
	}
	assert.True(t, st.Oldest.Equal(min), "Oldest %v should equal min DeletedAt %v", *st.Oldest, min)
}

func TestStatusUnknownShareReturnsNotFound(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	_, err := tt.svc.Status(tt.ctx, "/nope")
	require.Error(t, err)
	assert.True(t, metadata.IsNotFoundError(err))
}
